package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	email    = getEnv("MAIL_EMAIL", "qq353324582@163.com")
	password = getEnv("MAIL_PASSWORD", "ETxmyN7D984VnizL")
	port     = getEnv("PORT", "20044")
)

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" { return v }
	return fallback
}

type Email struct {
	UID          string `json:"uid"`
	Subject      string `json:"subject"`
	Sender       string `json:"sender"`
	Date         string `json:"date"`
	Timestamp    string `json:"timestamp"`
	Body         string `json:"body"`
	BodyHTML     string `json:"body_html,omitempty"`
	IsRead       bool   `json:"is_read"`
	IsUnreadIMAP bool   `json:"is_unread_imap"`
}

var (
	cache     []Email
	cacheMu   sync.RWMutex
	lastPoll  string
	unreadCnt int
)

type imapConn struct {
	conn net.Conn
	br   *bufio.Reader
}

func dialIMAP() (*imapConn, error) {
	tcp, err := net.DialTimeout("tcp", "imap.163.com:993", 30*time.Second)
	if err != nil { return nil, err }
	tlsCfg := &tls.Config{ServerName: "imap.163.com"}
	tlsConn := tls.Client(tcp, tlsCfg)
	if err := tlsConn.Handshake(); err != nil { return nil, err }
	c := &imapConn{conn: tlsConn, br: bufio.NewReader(tlsConn)}
	c.readLine()
	return c, nil
}

func (c *imapConn) send(format string, args ...interface{}) {
	fmt.Fprintf(c.conn, format+"\r\n", args...)
}

func (c *imapConn) readLine() (string, error) {
	l, err := c.br.ReadString('\n')
	if err != nil { return "", err }
	return strings.TrimRight(l, "\r\n"), nil
}

// readResp 读取 IMAP 响应直到遇到 tagged 响应，正确处理 literal
func (c *imapConn) readResp(tag string) ([]string, error) {
	var all []string
	for {
		l, err := c.readLine()
		if err != nil { return all, err }
		all = append(all, l)
		if strings.HasPrefix(l, tag) { return all, nil }
		// 检查是否是 literal
		if m := regexp.MustCompile(`\{(\d+)\}$`).FindStringSubmatch(l); m != nil {
			size, _ := strconv.Atoi(m[1])
			buf := make([]byte, size)
			if _, err := io.ReadFull(c.br, buf); err != nil { return all, err }
			all = append(all, string(buf))
			c.readLine()
		}
	}
}

func decodeMIME(s string) string {
	if !strings.Contains(s, "=?") { return s }
	d, err := new(mime.WordDecoder).DecodeHeader(s)
	if err != nil { return s }
	return d
}

func stripHTML(s string) string {
	if s == "" { return "" }
	// 先移除 style 和 script 块（Python 版没有这个，但应该加）
	s = regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`).ReplaceAllString(s, "")
	// 替换换行元素
	s = strings.NewReplacer("<br>", "\n", "<br/>", "\n", "<br />", "\n").Replace(s)
	s = regexp.MustCompile(`</p>`).ReplaceAllString(s, "\n")
	s = regexp.MustCompile(`</div>`).ReplaceAllString(s, "\n")
	s = regexp.MustCompile(`</tr>`).ReplaceAllString(s, "\n")
	s = regexp.MustCompile(`</td>`).ReplaceAllString(s, " ")
	s = regexp.MustCompile(`<li>`).ReplaceAllString(s, "\n- ")
	// 去掉所有 HTML 标签
	s = regexp.MustCompile(`<[^>]+>`).ReplaceAllString(s, "")
	// 解码 HTML 实体
	s = html.UnescapeString(s)
	// 清理空白
	s = regexp.MustCompile(`\n\s*\n+`).ReplaceAllString(s, "\n\n")
	s = regexp.MustCompile(`[ 	]+`).ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// decodeBody 根据 Content-Transfer-Encoding 解码 body
func decodeBody(body []byte, encoding string) ([]byte, error) {
	switch strings.ToLower(encoding) {
	case "base64":
		clean := strings.NewReplacer("\n", "", "\r", "", " ", "").Replace(string(body))
		return base64.StdEncoding.DecodeString(clean)
	case "quoted-printable":
		return laxQPDecode(body), nil
	default:
		// 尝试 QP 解码，如果生成长度不同则有效
		decoded := laxQPDecode(body)
		if len(decoded) != len(body) {
			return decoded, nil
		}
		return body, nil
	}
}

// removeOrphanEquals 移除解码后残留的 = 符号（QP软换行标记）
func removeOrphanEquals(data []byte) []byte {
	// 安全的：HTML/CSS 中 '= ' 总是 QP 软换行残留
	// 正常 HTML 中 '=' 后面跟 '"' 或字母，不是空格
	return bytes.ReplaceAll(data, []byte("= "), []byte{})
}

// laxQPDecode 宽容的 QP 解码器 — 处理不标准的软换行格式 (= =)
func laxQPDecode(data []byte) []byte {
	var result []byte
	i := 0
	for i < len(data) {
		if data[i] == '=' && i+2 < len(data) && isHex(data[i+1]) && isHex(data[i+2]) {
			b, _ := strconv.ParseUint(string(data[i+1:i+3]), 16, 8)
			result = append(result, byte(b))
			i += 3
		} else if data[i] == '=' {
			// 软换行：= 后跟可选空格/tab/\r，再跟 \n
			// 标准格式: =\r\n =\n
			// 非标准格式: = \n (空格+换行)，邮件中常见
			j := i + 1
			// 跳过空格、tab、\r
			for j < len(data) && (data[j] == ' ' || data[j] == '	' || data[j] == '\r') {
				j++
			}
			if j < len(data) && data[j] == '\n' {
				i = j + 1
			} else {
				result = append(result, data[i])
				i++
			}
		} else {
			result = append(result, data[i])
			i++
		}
	}
	return result
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'A' && c <= 'F') || (c >= 'a' && c <= 'f')
}

// extractTextBody 使用标准 MIME 解析提取正文（与 Python 版逻辑一致）
func extractTextBody(rawEmail string) string {
	// 先合并折叠头
	raw := mergeHeaders(rawEmail)

	// 分离头部和正文
	idx := strings.Index(raw, "\n\n")
	if idx < 0 { return "" }
	headerText := raw[:idx]
	bodyText := raw[idx+2:]

	// 解析 Content-Type
	ct := "text/plain"
	charset := "utf-8"
	boundary := ""
	for _, line := range strings.Split(headerText, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "content-type:") {
			// 用 mime 包解析媒体类型
			mt, params, err := mime.ParseMediaType(strings.TrimPrefix(line, "Content-Type:"))
			if err == nil {
				ct = mt
				if c, ok := params["charset"]; ok { charset = c }
				if b, ok := params["boundary"]; ok { boundary = b }
			}
		}
	}

	// 如果没找到 encoding 则从头部找
	encoding := ""
	for _, line := range strings.Split(headerText, "\n") {
		if strings.HasPrefix(strings.ToLower(line), "content-transfer-encoding:") {
			encoding = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(line), "content-transfer-encoding:"))
		}
	}

	// 处理 multipart
	var htmlFallback string
	if strings.HasPrefix(ct, "multipart/") && boundary != "" {
		mr := multipart.NewReader(strings.NewReader(bodyText), boundary)
		for {
			p, err := mr.NextPart()
			if err != nil { break }
			
			partCt := p.Header.Get("Content-Type")
			partEnc := p.Header.Get("Content-Transfer-Encoding")
			body, _ := io.ReadAll(p)

			// 解码
			decoded, err := decodeBody(body, partEnc)
			if err != nil { decoded = body }

			mt, _, _ := mime.ParseMediaType(partCt)
			switch mt {
			case "text/plain":
				result := strings.TrimSpace(string(decoded))
				if len(result) > 1000 { result = result[:1000] }
				return result
			case "text/html":
				if extracted := extractFromHTML(decoded, charset); extracted != "" {
					htmlFallback = extracted
				}
			}
		}
		if htmlFallback != "" { return htmlFallback }
		return ""
	}

	// 非 multipart
	switch {
	case strings.HasPrefix(ct, "text/html"):
		decoded, err := decodeBody([]byte(bodyText), encoding)
		if err != nil { return "" }
		return stripHTML(string(decoded))
	default:
		decoded, err := decodeBody([]byte(bodyText), encoding)
		if err != nil { return "" }
		result := string(decoded)
		if len(result) > 1000 { result = result[:1000] }
		return strings.TrimSpace(result)
	}
}

func extractFromHTML(data []byte, charset string) string {
	text := string(data)
	text = stripHTML(text)
	if len(text) > 1000 { text = text[:1000] }
	return strings.TrimSpace(text)
}

// extractRawHTML 提取原始 HTML 正文（与 Python 版的 body_html 对应）
func extractRawHTML(rawEmail string) string {
	raw := mergeHeaders(rawEmail)
	idx := strings.Index(raw, "\n\n")
	if idx < 0 { return "" }
	bodyText := raw[idx+2:]

	// 分离头部
	headerText := raw[:idx]
	ct := "text/plain"
	boundary := ""
	encoding := ""
	for _, line := range strings.Split(headerText, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "content-type:") {
			mt, params, err := mime.ParseMediaType(strings.TrimPrefix(line, "Content-Type:"))
			if err == nil {
				ct = mt
				if b, ok := params["boundary"]; ok { boundary = b }
			}
		}
		if strings.HasPrefix(strings.ToLower(line), "content-transfer-encoding:") {
			encoding = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(line), "content-transfer-encoding:"))
		}
	}

	// 如果是 HTML，解码后返回（截断到 50000）
	if strings.HasPrefix(ct, "text/html") {
		decoded, err := decodeBody([]byte(bodyText), encoding)
		if err != nil { decoded = []byte(bodyText) }
		result := string(decoded)
		result = string(removeOrphanEquals([]byte(result)))
		result = cleanURLs(result)
		if len(result) > 500000 { result = result[:500000] }
		return result
	}

	// 如果是 multipart，找 HTML 部分
	if strings.HasPrefix(ct, "multipart/") && boundary != "" {
		mr := multipart.NewReader(strings.NewReader(bodyText), boundary)
		for {
			p, err := mr.NextPart()
			if err != nil { break }
			partCt := p.Header.Get("Content-Type")
			partEnc := p.Header.Get("Content-Transfer-Encoding")
			body, _ := io.ReadAll(p)

			mt, _, _ := mime.ParseMediaType(partCt)
			if mt == "text/html" {
				decoded, err := decodeBody(body, partEnc)
				if err != nil { decoded = body }
				result := string(decoded)
				result = string(removeOrphanEquals([]byte(result)))
				result = cleanURLs(result)
				if len(result) > 500000 { result = result[:500000] }
				return result
			}
		}
	}

	// 非 HTML：返回和 body 一样的纯文本
	return extractTextBody(rawEmail)
}

// cleanURLs 清理图片链接里的无效字符（如邮件自带的 \x01-\x1f 控制字符）
func cleanURLs(html string) string {
	re := regexp.MustCompile(`src="[^"]*"`)
	return re.ReplaceAllStringFunc(html, func(src string) string {
		url := src[5 : len(src)-1]
		clean := strings.Map(func(r rune) rune {
			if r >= 0x00 && r <= 0x1f {
				return -1
			}
			return r
		}, url)
		return `src="` + clean + `"`
	})
}

func parseDate(s string) string {
	// 清理日期字符串中的额外格式
	s = strings.TrimSpace(s)
	// 移除 (UTC)、(CST)、(PDT) 等额外文本
	if idx := strings.Index(s, "("); idx > 0 {
		s = strings.TrimSpace(s[:idx])
	}
	// 标准 RFC 1123 格式
	for _, f := range []string{
		"Mon, 02 Jan 2006 15:04:05 -0700",
		"Mon, 2 Jan 2006 15:04:05 -0700",
		"02 Jan 2006 15:04:05 -0700",
		"Mon, 02 Jan 2006 15:04:05 MST",
		"Mon, 02 Jan 2006 15:04:05 Z0700",
		"Mon, 2 Jan 2006 15:04:05 Z0700",
		"2006-01-02 15:04:05 -0700",
		"2006-01-02T15:04:05-0700",
		"2006-01-02T15:04:05Z",
	} {
		if t, err := time.Parse(f, s); err == nil {
			return t.Format(time.RFC3339)
		}
	}
	return time.Now().Format(time.RFC3339)
}

func mergeHeaders(raw string) string {
	lines := strings.Split(raw, "\n")
	var r []string
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if len(l) > 0 && (l[0] == ' ' || l[0] == '\t') && len(r) > 0 {
			r[len(r)-1] += " " + strings.TrimSpace(l)
		} else { r = append(r, l) }
	}
	return strings.Join(r, "\n")
}

var tagCounter int

func pollEmails() {
	mail, err := dialIMAP()
	if err != nil { log.Printf("连接失败: %v", err); return }
	defer mail.conn.Close()

	// 必须发 ID 命令（163 邮箱限制）
	tagCounter++
	mail.send("a%04d ID (\"name\" \"Microsoft Outlook\" \"version\" \"16.0\")", tagCounter)
	mail.readResp(fmt.Sprintf("a%04d", tagCounter))

	tagCounter++
	mail.send("a%04d LOGIN %s %s", tagCounter, email, password)
	resp, _ := mail.readResp(fmt.Sprintf("a%04d", tagCounter))
	if !strings.Contains(resp[len(resp)-1], "OK") { log.Printf("登录失败"); return }

	tagCounter++
	mail.send("a%04d SELECT INBOX", tagCounter)
	resp, _ = mail.readResp(fmt.Sprintf("a%04d", tagCounter))
	if !strings.Contains(resp[len(resp)-1], "OK") { log.Printf("select失败"); return }

	tagCounter++
	mail.send("a%04d UID SEARCH ALL", tagCounter)
	resp, _ = mail.readResp(fmt.Sprintf("a%04d", tagCounter))
	var uids []uint32
	for _, l := range resp {
		if strings.HasPrefix(l, "* SEARCH") {
			for _, p := range strings.Fields(l)[2:] {
				if uid, err := strconv.ParseUint(p, 10, 32); err == nil { uids = append(uids, uint32(uid)) }
			}
		}
	}
	if len(uids) == 0 { return }
	if len(uids) > 20 { uids = uids[len(uids)-20:] }

	cacheMu.RLock()
	existing := make(map[uint32]bool)
	for _, e := range cache {
		var uid uint32
		fmt.Sscanf(e.UID, "%d", &uid)
		existing[uid] = true
	}
	cacheMu.RUnlock()

	var newEmails []Email
	for _, uid := range uids {
		if existing[uid] { continue }
		tagCounter++
		mail.send("a%04d UID FETCH %d (BODY[] FLAGS)", tagCounter, uid)
		resp, _ := mail.readResp(fmt.Sprintf("a%04d", tagCounter))

		// 找到最长的响应元素（就是邮件正文 literal）
		rawEmail := ""
		isUnread := true
		for _, l := range resp {
			if strings.Contains(l, "\\Seen") { isUnread = false }
			if len(l) > len(rawEmail) { rawEmail = l }
		}
		if rawEmail == "" { continue }

		merged := mergeHeaders(rawEmail)
		var hdrs []string
		inH := true
		for _, l := range strings.Split(merged, "\n") {
			l = strings.TrimRight(l, "\r")
			if inH { if l == "" { inH = false } else { hdrs = append(hdrs, l) } }
		}
		subj, from, date := "", "", ""
		for _, l := range hdrs {
			if idx := strings.Index(l, ":"); idx > 0 {
				k := strings.ToLower(strings.TrimSpace(l[:idx]))
				if k == "subject" { subj = decodeMIME(strings.TrimSpace(l[idx+1:])) }
				if k == "from" { from = decodeMIME(strings.TrimSpace(l[idx+1:])) }
				if k == "date" { date = strings.TrimSpace(l[idx+1:]) }
			}
		}
		body := extractTextBody(rawEmail)
		bodyHTML := extractRawHTML(rawEmail)
		newEmails = append(newEmails, Email{
			UID: fmt.Sprintf("%d", uid), Subject: subj, Sender: from,
			Date: date, Timestamp: parseDate(date), Body: body, BodyHTML: bodyHTML,
			IsRead: !isUnread, IsUnreadIMAP: isUnread,
		})
	}

	if len(newEmails) > 0 {
		cacheMu.Lock()
		cache = append(cache, newEmails...)
		sort.Slice(cache, func(i, j int) bool { return cache[i].Timestamp > cache[j].Timestamp })
		if len(cache) > 20 { cache = cache[:20] }
		unreadCnt = 0
		for _, e := range cache { if !e.IsRead { unreadCnt++ } }
		lastPoll = time.Now().Format(time.RFC3339)
		cacheMu.Unlock()
		log.Printf("新增 %d 封，未读 %d，共 %d 封", len(newEmails), unreadCnt, len(cache))
	}
}

func pollLoop() {
	for { pollEmails(); time.Sleep(60 * time.Second) }
}

func apiResp(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(data)
}

func handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
	if r.Method == "OPTIONS" { return }
	p := r.URL.Path
	switch {
	case p == "/api/unread-count":
		cacheMu.RLock()
		apiResp(w, map[string]interface{}{"unread_count": unreadCnt, "last_poll": lastPoll})
		cacheMu.RUnlock()
	case p == "/api/emails":
		cacheMu.RLock()
		apiResp(w, map[string]interface{}{"emails": cache, "unread_count": unreadCnt, "last_poll": lastPoll})
		cacheMu.RUnlock()
	case strings.HasPrefix(p, "/api/emails/") && r.Method == "GET":
		uid := strings.TrimPrefix(p, "/api/emails/")
		cacheMu.RLock()
		var found *Email
		for i := range cache { if cache[i].UID == uid { found = &cache[i]; break } }
		cacheMu.RUnlock()
		if found != nil { apiResp(w, found) } else { w.WriteHeader(404); apiResp(w, map[string]string{"error": "not found"}) }
	case strings.HasPrefix(p, "/api/mark-read/") && r.Method == "POST":
		uid := strings.TrimPrefix(p, "/api/mark-read/")
		cacheMu.Lock()
		for i := range cache { if cache[i].UID == uid { cache[i].IsRead = true; break } }
		unreadCnt = 0
		for _, e := range cache { if !e.IsRead { unreadCnt++ } }
		cacheMu.Unlock()
		apiResp(w, map[string]bool{"success": true})
	default: w.WriteHeader(404); apiResp(w, map[string]string{"error": "not found"})
	}
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	log.Printf("服务启动，邮箱 %s，端口 %s", email, port)
	go func() { pollEmails(); for { time.Sleep(60 * time.Second); pollEmails() } }()
	http.HandleFunc("/", handler)
	log.Printf("HTTP 服务已启动: http://0.0.0.0:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
