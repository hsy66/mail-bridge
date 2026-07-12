package main

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
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

type imapConn struct {
	conn net.Conn
	br   *bufio.Reader
	tag  int
}

func dialIMAP() (*imapConn, error) {
	tcp, err := net.DialTimeout("tcp", "imap.163.com:993", 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial: %w", err)
	}
	tlsCfg := &tls.Config{ServerName: "imap.163.com"}
	tlsConn := tls.Client(tcp, tlsCfg)
	if err := tlsConn.Handshake(); err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	c := &imapConn{conn: tlsConn, br: bufio.NewReader(tlsConn), tag: 1}
	// 读欢迎
	_, _ = c.readLine()
	return c, nil
}

func (c *imapConn) cmd(format string, args ...interface{}) string {
	c.tag++
	tag := fmt.Sprintf("a%04d", c.tag)
	line := fmt.Sprintf(format, args...)
	_, err := fmt.Fprintf(c.conn, "%s %s\r\n", tag, line)
	if err != nil {
		return ""
	}
	return tag
}

func (c *imapConn) readLine() (string, error) {
	line, err := c.br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (c *imapConn) readUntil(tag string) ([]string, error) {
	var lines []string
	for {
		line, err := c.readLine()
		if err != nil {
			return lines, err
		}
		if strings.HasPrefix(line, tag) {
			lines = append(lines, line)
			return lines, nil
		}
		if strings.HasPrefix(line, "*") || strings.HasPrefix(line, "+") {
			lines = append(lines, line)
		}
	}
}

func (c *imapConn) readUntilTag(tag string) (string, error) {
	for {
		line, err := c.readLine()
		if err != nil {
			return "", err
		}
		if strings.HasPrefix(line, tag) {
			return line, nil
		}
	}
}

func (c *imapConn) close() {
	c.conn.Close()
}

func pollEmails() {
	mail, err := dialIMAP()
	if err != nil {
		log.Printf("连接失败: %v", err)
		return
	}
	defer mail.close()

	// 先发 ID 命令（163 邮箱需要）
	mail.cmd("ID (\"name\" \"Microsoft Outlook\" \"version\" \"16.0\")")
	mail.readUntilTag("a")

	// 登录
	tag := mail.cmd("LOGIN %s %s", email, password)
	_, err = mail.readUntil(tag)
	if err != nil {
		log.Printf("登录失败: %v", err)
		return
	}

	// Select INBOX
	tag = mail.cmd("SELECT INBOX")
	lines, err := mail.readUntil(tag)
	if err != nil {
		log.Printf("select失败: %v", err)
		return
	}
	// 确认 OK
	last := lines[len(lines)-1]
	if strings.Contains(last, "NO") {
		log.Printf("select被拒: %s", last)
		return
	}

	// 搜索所有邮件
	tag = mail.cmd("UID SEARCH ALL")
	lines, err = mail.readUntil(tag)
	if err != nil {
		return
	}

	// 解析 UID 列表
	var allUIDs []uint32
	for _, l := range lines {
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

	// 只取最近 20 封
	if len(allUIDs) > 20 {
		allUIDs = allUIDs[len(allUIDs)-20:]
	}

	// 获取已有 UID
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

		// 获取邮件
		tag = mail.cmd("UID FETCH %d (BODY[] FLAGS)", uid)
		lines, _ = mail.readUntil(tag)

		var rawBody string
		inBody := false
		isUnread := true
		for _, l := range lines {
			if strings.Contains(l, "\\Seen") {
				isUnread = false
			}
			if strings.Contains(l, "BODY[") {
				inBody = true
				continue
			}
			if inBody {
				if strings.HasPrefix(l, ")") || strings.HasPrefix(l, "a") || l == "" {
					break
				}
				rawBody += l + "\n"
			}
		}

		subject, from, date, body := parseEmail(rawBody)
		ts := parseDate(date)
		if len(body) > 1000 {
			body = body[:1000]
		}

		newEmails = append(newEmails, Email{
			UID:          fmt.Sprintf("%d", uid),
			Subject:      subject,
			Sender:       from,
			Date:         date,
			Timestamp:    ts,
			Body:         stripHTML(body),
			IsRead:       !isUnread,
			IsUnreadIMAP: isUnread,
		})
	}

	if len(newEmails) > 0 {
		cacheMu.Lock()
		for _, e := range newEmails {
			cache = append(cache, e)
		}
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

func parseEmail(raw string) (subject, from, date, body string) {
	lines := strings.Split(raw, "\n")
	inHeaders := true
	var headerLines, bodyLines []string
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
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

	return headers["subject"], headers["from"], headers["date"], strings.Join(bodyLines, "\n")
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

func stripHTML(s string) string {
	var r strings.Builder
	inTag := false
	for _, c := range s {
		if c == '<' {
			inTag = true
		} else if c == '>' {
			inTag = false
		} else if !inTag {
			r.WriteRune(c)
		}
	}
	return strings.TrimSpace(r.String())
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
			"unread_count": unreadCnt,
			"last_poll":    lastPoll,
		})
		cacheMu.RUnlock()

	case p == "/api/emails":
		cacheMu.RLock()
		apiResp(w, map[string]interface{}{
			"emails":       cache,
			"unread_count": unreadCnt,
			"last_poll":    lastPoll,
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
