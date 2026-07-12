package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"mime"
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
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

type Email struct {
	UID          string `json:"uid"`
	Subject      string `json:"subject"`
	Sender       string `json:"sender"`
	Date         string `json:"date"`
	Timestamp    string `json:"timestamp"`
	Body         string `json:"body"`
	IsRead       bool   `json:"is_read"`
	IsUnreadIMAP bool   `json:"is_unread_imap"`
}

var (
	cache     []Email
	cacheMu   sync.RWMutex
	lastPoll  string
	unreadCnt int
)

// ========== IMAP 连接（正确处理 Literal） ==========
type imapConn struct {
	conn net.Conn
	br   *bufio.Reader
	tag  int
}

func dialIMAP() (*imapConn, error) {
	tcp, err := net.DialTimeout("tcp", "imap.163.com:993", 30*time.Second)
	if err != nil {
		return nil, err
	}
	tlsCfg := &tls.Config{ServerName: "imap.163.com"}
	tlsConn := tls.Client(tcp, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		return nil, err
	}
	c := &imapConn{conn: tlsConn, br: bufio.NewReader(tlsConn), tag: 1}
	c.readLine() // read greeting
	return c, nil
}

func (c *imapConn) cmd(format string, args ...interface{}) string {
	c.tag++
	tag := fmt.Sprintf("a%04d", c.tag)
	line := fmt.Sprintf(format, args...)
	fmt.Fprintf(c.conn, "%s %s\r\n", tag, line)
	return tag
}

func (c *imapConn) readLine() (string, error) {
	line, err := c.br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// readResponse 读取 IMAP 响应，正确处理 Literal {size}
func (c *imapConn) readResponses(tag string) ([]string, error) {
	var all []string
	literalRe := regexp.MustCompile(`\{(\d+)\}$`)
	for {
		line, err := c.readLine()
		if err != nil {
			return all, err
		}
		// 检查是否 literal
		if m := literalRe.FindStringSubmatch(line); m != nil {
			size, _ := strconv.Atoi(m[1])
			// 读 literal 数据
			literal := make([]byte, size)
			_, err := c.br.Read(literal)
			if err != nil {
				return all, err
			}
			all = append(all, line)
			all = append(all, string(literal))
			// 读 literal 后的换行
			c.readLine()
		} else {
			all = append(all, line)
		}
		if strings.HasPrefix(line, tag+" ") || strings.HasPrefix(line, tag+" OK") || strings.HasPrefix(line, tag+" NO") || strings.HasPrefix(line, tag+" BAD") {
			return all, nil
		}
	}
}

// ========== MIME 解码 ==========
func decodeMIMEHeader(s string) string {
	if !strings.Contains(s, "=?") {
		return s
	}
	dec := new(mime.WordDecoder)
	d, err := dec.DecodeHeader(s)
	if err != nil {
		return s
	}
	return d
}

// ========== 邮件解析 ==========
func parseEmail(raw string) (subject, from, date, body string) {
	// 合并折叠头
	lines := strings.Split(raw, "\n")
	var merged []string
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if len(l) > 0 && (l[0] == ' ' || l[0] == '\t') && len(merged) > 0 {
			merged[len(merged)-1] += " " + strings.TrimSpace(l)
		} else {
			merged = append(merged, l)
		}
	}

	inHeaders := true
	var headerLines, bodyLines []string
	for _, l := range merged {
		if inHeaders {
			if l == "" {
				inHeaders = false
				continue
			}
			headerLines = append(headerLines, l)
		} else {
			bodyLines = append(bodyLines, l)
		}
	}

	headers := make(map[string]string)
	for _, l := range headerLines {
		if idx := strings.Index(l, ":"); idx > 0 {
			k := strings.ToLower(strings.TrimSpace(l[:idx]))
			v := strings.TrimSpace(l[idx+1:])
			headers[k] = v
		}
	}

	subject = decodeMIMEHeader(headers["subject"])
	from = decodeMIMEHeader(headers["from"])
	date = headers["date"]
	rawBody := strings.Join(bodyLines, "\n")

	// 提取正文（优先 text/plain）
	body = extractTextBody(rawBody)
	return
}

func extractTextBody(raw string) string {
	// 从 multipart 中找 text/plain 或 text/html
	plainIdx := strings.Index(raw, "Content-Type: text/plain")
	htmlIdx := strings.Index(raw, "Content-Type: text/html")

	var targetIdx int
	isHTML := false
	if plainIdx >= 0 {
		targetIdx = plainIdx
	} else if htmlIdx >= 0 {
		targetIdx = htmlIdx
		isHTML = true
	} else {
		if len(raw) > 1000 {
			return raw[:1000]
		}
		return raw
	}

	body := raw[targetIdx:]
	lines := strings.Split(body, "\n")
	started := false
	var textLines []string
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if !started {
			if l == "" {
				started = true
			}
			continue
		}
		if strings.HasPrefix(l, "--") || strings.HasPrefix(l, "Content-") {
			break
		}
		textLines = append(textLines, l)
	}

	result := strings.Join(textLines, "\n")
	if isHTML {
		var r strings.Builder
		inTag := false
		for _, c := range result {
			if c == '<' {
				inTag = true
			} else if c == '>' {
				inTag = false
			} else if !inTag {
				r.WriteRune(c)
			}
		}
		result = r.String()
	}
	return strings.TrimSpace(result)
}

func parseDate(s string) string {
	formats := []string{
		"Mon, 02 Jan 2006 15:04:05 -0700",
		"Mon, 2 Jan 2006 15:04:05 -0700",
		"02 Jan 2006 15:04:05 -0700",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t.Format(time.RFC3339)
		}
	}
	return time.Now().Format(time.RFC3339)
}

// ========== IMAP 轮询 ==========
func pollEmails() {
	mail, err := dialIMAP()
	if err != nil {
		log.Printf("连接失败: %v", err)
		return
	}
	defer mail.conn.Close()

	// ID + 登录
	tag := mail.cmd(`ID ("name" "Microsoft Outlook" "version" "16.0")`)
	mail.readResponses(tag)

	tag = mail.cmd("LOGIN %s %s", email, password)
	resp, err := mail.readResponses(tag)
	if err != nil {
		log.Printf("登录失败: %v", err)
		return
	}
	if !strings.Contains(resp[len(resp)-1], "OK") {
		log.Printf("登录被拒")
		return
	}

	tag = mail.cmd("SELECT INBOX")
	resp, err = mail.readResponses(tag)
	if err != nil || !strings.Contains(resp[len(resp)-1], "OK") {
		log.Printf("select失败")
		return
	}

	// 搜索
	tag = mail.cmd("UID SEARCH ALL")
	resp, err = mail.readResponses(tag)
	if err != nil {
		return
	}

	var allUIDs []uint32
	for _, l := range resp {
		if strings.HasPrefix(l, "* SEARCH") {
			parts := strings.Fields(l)
			for _, p := range parts[2:] {
				if uid, err := strconv.ParseUint(p, 10, 32); err == nil {
					allUIDs = append(allUIDs, uint32(uid))
				}
			}
		}
	}
	if len(allUIDs) == 0 {
		return
	}
	if len(allUIDs) > 20 {
		allUIDs = allUIDs[len(allUIDs)-20:]
	}

	cacheMu.RLock()
	existing := make(map[uint32]bool)
	for _, e := range cache {
		if uid, err := strconv.ParseUint(e.UID, 10, 32); err == nil {
			existing[uint32(uid)] = true
		}
	}
	cacheMu.RUnlock()

	var newEmails []Email
	for _, uid := range allUIDs {
		if existing[uid] {
			continue
		}

		tag = mail.cmd("UID FETCH %d (BODY[] FLAGS)", uid)
		resp, err = mail.readResponses(tag)
		if err != nil {
			continue
		}

		// 收集 literal 数据（即邮件正文）
		var rawEmail string
		isUnread := true
		for _, l := range resp {
			if strings.Contains(l, "\\Seen") {
				isUnread = false
			}
			// literal 数据是原始邮件，包含所有头部和正文
			if strings.HasPrefix(l, "Received:") || strings.HasPrefix(l, "From:") ||
				strings.HasPrefix(l, "To:") || strings.HasPrefix(l, "Subject:") ||
				strings.HasPrefix(l, "Date:") || strings.HasPrefix(l, "Message-ID:") ||
				strings.HasPrefix(l, "MIME-Version:") || strings.HasPrefix(l, "Content-Type:") ||
				strings.HasPrefix(l, "Content-Transfer-") || strings.HasPrefix(l, "DKIM-") ||
				strings.HasPrefix(l, "X-") || strings.HasPrefix(l, "Authentication-") ||
				strings.HasPrefix(l, "Received:") || strings.HasPrefix(l, "Return-") ||
				strings.HasPrefix(l, "Delivered-") || strings.HasPrefix(l, "List-") ||
				strings.HasPrefix(l, "Feedback-ID:") || strings.HasPrefix(l, "In-Reply-To:") ||
				strings.HasPrefix(l, "References:") || strings.HasPrefix(l, "Reply-To:") ||
				strings.HasPrefix(l, "Thread-") || strings.HasPrefix(l, "Archived-At:") ||
				strings.HasPrefix(l, "Accept-Language:") || strings.HasPrefix(l, "Content-Language:") ||
				strings.HasPrefix(l, "Sensitivity:") || strings.HasPrefix(l, "Precedence:") ||
				strings.HasPrefix(l, "Auto-Submitted:") || strings.HasPrefix(l, "Priority:") ||
				strings.HasPrefix(l, "Importance:") || strings.HasPrefix(l, "X-Mailer:") ||
				strings.HasPrefix(l, "X-Priority:") || strings.HasPrefix(l, "X-MSMail-") ||
				strings.HasPrefix(l, "User-Agent:") || l == "" {
				rawEmail += l + "\n"
			}
		}

		if rawEmail == "" {
			// FETCH 响应可能是 literal 在前
			for i, l := range resp {
				if strings.Contains(l, "{") && i+1 < len(resp) {
					rawEmail = resp[i+1]
					break
				}
			}
		}

		subject, from, date, body := parseEmail(rawEmail)

		newEmails = append(newEmails, Email{
			UID:          fmt.Sprintf("%d", uid),
			Subject:      subject,
			Sender:       from,
			Date:         date,
			Timestamp:    parseDate(date),
			Body:         body,
			IsRead:       !isUnread,
			IsUnreadIMAP: isUnread,
		})
	}

	if len(newEmails) > 0 {
		cacheMu.Lock()
		cache = append(cache, newEmails...)
		sort.Slice(cache, func(i, j int) bool {
			return cache[i].Timestamp > cache[j].Timestamp
		})
		if len(cache) > 20 {
			cache = cache[:20]
		}
		unreadCnt = 0
		for _, e := range cache {
			if !e.IsRead {
				unreadCnt++
			}
		}
		lastPoll = time.Now().Format(time.RFC3339)
		cacheMu.Unlock()
		log.Printf("新增 %d 封，未读 %d，共 %d 封", len(newEmails), unreadCnt, len(cache))
	}
}

func pollLoop() {
	for {
		pollEmails()
		time.Sleep(60 * time.Second)
	}
}

// ========== HTTP ==========
func apiResp(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(data)
}

func handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
	if r.Method == "OPTIONS" {
		return
	}
	p := r.URL.Path

	switch {
	case p == "/api/unread-count":
		cacheMu.RLock()
		apiResp(w, map[string]interface{}{
			"unread_count": unreadCnt, "last_poll": lastPoll,
		})
		cacheMu.RUnlock()

	case p == "/api/emails":
		cacheMu.RLock()
		apiResp(w, map[string]interface{}{
			"emails": cache, "unread_count": unreadCnt, "last_poll": lastPoll,
		})
		cacheMu.RUnlock()

	case strings.HasPrefix(p, "/api/emails/") && r.Method == "GET":
		uid := strings.TrimPrefix(p, "/api/emails/")
		cacheMu.RLock()
		var found *Email
		for i := range cache {
			if cache[i].UID == uid {
				found = &cache[i]
				break
			}
		}
		cacheMu.RUnlock()
		if found != nil {
			apiResp(w, found)
		} else {
			w.WriteHeader(404)
			apiResp(w, map[string]string{"error": "not found"})
		}

	case strings.HasPrefix(p, "/api/mark-read/") && r.Method == "POST":
		uid := strings.TrimPrefix(p, "/api/mark-read/")
		cacheMu.Lock()
		for i := range cache {
			if cache[i].UID == uid {
				cache[i].IsRead = true
				break
			}
		}
		unreadCnt = 0
		for _, e := range cache {
			if !e.IsRead {
				unreadCnt++
			}
		}
		cacheMu.Unlock()
		apiResp(w, map[string]bool{"success": true})

	default:
		w.WriteHeader(404)
		apiResp(w, map[string]string{"error": "not found"})
	}
}

func main() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	log.Printf("服务启动，邮箱 %s，端口 %s", email, port)

	go func() {
		pollEmails()
		for {
			time.Sleep(60 * time.Second)
			pollEmails()
		}
	}()

	http.HandleFunc("/", handler)
	log.Printf("HTTP 服务已启动: http://0.0.0.0:%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}
