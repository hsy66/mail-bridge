package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime"
	"mime/quotedprintable"
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

var boundaryRe = regexp.MustCompile(`boundary="?([^"\s;]+)"?`)

func extractTextBody(raw string) string {
	parts := []string{raw}
	if m := boundaryRe.FindStringSubmatch(raw); m != nil {
		parts = strings.Split(raw, "--"+m[1])
	}
	for _, part := range parts {
		lower := strings.ToLower(part)
		isPlain := strings.Contains(lower, "content-type: text/plain")
		isHTML := strings.Contains(lower, "content-type: text/html")
		if !isPlain && !isHTML { continue }
		encoding := ""
		for _, l := range strings.Split(part, "\n") {
			l = strings.TrimRight(l, "\r")
			if strings.HasPrefix(strings.ToLower(l), "content-transfer-encoding:") {
				encoding = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(l), "content-transfer-encoding:"))
			}
		}
		var bl []string
		started := false
		for _, l := range strings.Split(part, "\n") {
			l = strings.TrimRight(l, "\r")
			if !started { if l == "" { started = true }; continue }
			if strings.HasPrefix(l, "--") || strings.HasPrefix(l, "Content-") { break }
			bl = append(bl, l)
		}
		result := strings.Join(bl, "\n")
		switch {
		case strings.Contains(encoding, "base64"):
			clean := strings.NewReplacer("\n", "", "\r", "").Replace(result)
			if d, err := base64.StdEncoding.DecodeString(clean); err == nil { result = string(d) }
		case strings.Contains(encoding, "quoted-printable"):
			if d, err := io.ReadAll(quotedprintable.NewReader(strings.NewReader(result))); err == nil { result = string(d) }
		}
		if isHTML && !isPlain {
			var sb strings.Builder
			inTag := false
			for _, c := range result { if c == '<' { inTag = true } else if c == '>' { inTag = false } else if !inTag { sb.WriteRune(c) } }
			result = sb.String()
		}
		result = strings.TrimSpace(result)
		if len(result) > 1000 { result = result[:1000] }
		if result != "" { return result }
	}
	return ""
}

func parseDate(s string) string {
	for _, f := range []string{"Mon, 02 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 -0700", "02 Jan 2006 15:04:05 -0700"} {
		if t, err := time.Parse(f, s); err == nil { return t.Format(time.RFC3339) }
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
		newEmails = append(newEmails, Email{
			UID: fmt.Sprintf("%d", uid), Subject: subj, Sender: from,
			Date: date, Timestamp: parseDate(date), Body: body,
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
