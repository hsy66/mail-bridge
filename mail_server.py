#!/usr/bin/env python3
import os
import json
import time
import re
import html as html_module
import email
import email.header
import email.utils
from datetime import datetime
from flask import Flask, jsonify
from threading import Thread
import logging
from imapclient import IMAPClient

app = Flask(__name__)

# ========== 配置（可通过环境变量覆盖） ==========
HARD_CODED_EMAIL = os.getenv("MAIL_EMAIL", "qq353324582@163.com")
HARD_CODED_PASSWORD = os.getenv("MAIL_PASSWORD", "ETxmyN7D984VnizL")
IMAP_SERVER = os.getenv("IMAP_SERVER", "imap.163.com")
IMAP_PORT = int(os.getenv("IMAP_PORT", "993"))
POLL_INTERVAL = int(os.getenv("POLL_INTERVAL", "60"))
MAX_EMAILS = int(os.getenv("MAX_EMAILS", "100"))

# ========== 运行配置 ==========
BASE_DIR = os.path.dirname(os.path.abspath(__file__))
CACHE_DIR = os.path.join(BASE_DIR, 'cache')
CACHE_FILE = os.path.join(CACHE_DIR, 'mails.json')
MAX_BODY_LEN = 1000
MAX_HTML_LEN = 50000

mail_cache = {"emails": [], "last_poll": None, "unread_count": 0}

@app.after_request
def after_request(response):
    response.headers.add('Access-Control-Allow-Origin', '*')
    response.headers.add('Access-Control-Allow-Headers', 'Content-Type')
    response.headers.add('Access-Control-Allow-Methods', 'GET,POST,OPTIONS')
    return response

def load_cache():
    global mail_cache
    if os.path.exists(CACHE_FILE):
        try:
            with open(CACHE_FILE, 'r', encoding='utf-8') as f:
                mail_cache = json.load(f)
        except Exception as e:
            logging.error(f"缓存加载失败，重建: {e}")
            mail_cache = {"emails": [], "last_poll": None, "unread_count": 0}

def save_cache():
    try:
        os.makedirs(CACHE_DIR, exist_ok=True)
        emails = mail_cache.get("emails", [])
        if len(emails) > MAX_EMAILS:
            emails.sort(key=lambda x: x.get("timestamp", ""), reverse=True)
            emails = emails[:MAX_EMAILS]
            mail_cache["emails"] = emails
        with open(CACHE_FILE, 'w', encoding='utf-8') as f:
            json.dump(mail_cache, f, ensure_ascii=False, indent=2)
    except Exception as e:
        logging.warning(f"缓存保存失败（不影响运行）: {e}")

def decode_str(s):
    if not s:
        return ""
    value, charset = email.header.decode_header(s)[0]
    if isinstance(value, bytes):
        return value.decode(charset or 'utf-8', errors='ignore')
    return value

def strip_html(html_content):
    if not html_content:
        return ""
    text = re.sub(r'<br\s*/?>', '\n', html_content, flags=re.IGNORECASE)
    text = re.sub(r'</p>', '\n', text, flags=re.IGNORECASE)
    text = re.sub(r'</div>', '\n', text, flags=re.IGNORECASE)
    text = re.sub(r'</tr>', '\n', text, flags=re.IGNORECASE)
    text = re.sub(r'</td>', ' ', text, flags=re.IGNORECASE)
    text = re.sub(r'<li>', '\n- ', text, flags=re.IGNORECASE)
    text = re.sub(r'<[^>]+>', '', text)
    text = html_module.unescape(text)
    text = re.sub(r'\n\s*\n+', '\n\n', text)
    text = re.sub(r'[ \t]+', ' ', text)
    return text.strip()

def get_body(msg):
    body = ""
    content_type = ""
    if msg.is_multipart():
        for part in msg.walk():
            ct = part.get_content_type()
            if ct == "text/plain":
                try:
                    payload = part.get_payload(decode=True)
                    charset = part.get_content_charset() or 'utf-8'
                    body = payload.decode(charset, errors='ignore')
                    content_type = "text/plain"
                    break
                except:
                    continue
        if not body:
            for part in msg.walk():
                ct = part.get_content_type()
                if ct == "text/html":
                    try:
                        payload = part.get_payload(decode=True)
                        charset = part.get_content_charset() or 'utf-8'
                        body = payload.decode(charset, errors='ignore')
                        content_type = "text/html"
                        break
                    except:
                        continue
    else:
        ct = msg.get_content_type()
        try:
            payload = msg.get_payload(decode=True)
            charset = msg.get_content_charset() or 'utf-8'
            body = payload.decode(charset, errors='ignore')
            content_type = ct
        except:
            pass
    return body, content_type

def poll_emails():
    try:
        mail = IMAPClient(IMAP_SERVER, port=IMAP_PORT, ssl=True, timeout=30)
        mail.id_({
            "name": "Microsoft Outlook",
            "version": "16.0.14326.20454",
            "vendor": "Microsoft Corporation",
            "os": "Windows",
            "os-version": "10.0.19044"
        })
        mail.login(HARD_CODED_EMAIL, HARD_CODED_PASSWORD)
        mail.select_folder('INBOX')

        messages = mail.search(['ALL'])
        if not messages:
            mail.logout()
            mail_cache['last_poll'] = datetime.now().isoformat()
            save_cache()
            return

        messages = messages[-50:]
        existing_uids = {e['uid'] for e in mail_cache['emails']}
        new_emails = []

        for uid in messages:
            uid_str = str(uid)
            if uid_str in existing_uids:
                continue
            data = mail.fetch([uid], ['RFC822', 'FLAGS'])
            if uid not in data:
                continue
            msg_data = data[uid]
            raw_email = msg_data[b'RFC822']
            flags = msg_data[b'FLAGS']
            msg = email.message_from_bytes(raw_email)
            subject = decode_str(msg['Subject'])
            sender = decode_str(msg['From'])
            date_str = msg['Date']
            try:
                parsed_date = email.utils.parsedate_to_datetime(date_str)
                timestamp = parsed_date.isoformat()
            except:
                timestamp = datetime.now().isoformat()
            body, ct = get_body(msg)
            body_preview = strip_html(body)[:MAX_BODY_LEN]
            body_html = body[:MAX_HTML_LEN] if ct == "text/html" else body_preview
            is_unread = b'\\Seen' not in flags
            email_obj = {
                "uid": uid_str, "subject": subject, "sender": sender,
                "date": date_str, "timestamp": timestamp,
                "body": body_preview, "body_html": body_html,
                "is_read": not is_unread, "is_unread_imap": is_unread
            }
            new_emails.append(email_obj)

        mail.logout()

        if new_emails:
            mail_cache['emails'].extend(new_emails)
            mail_cache['emails'].sort(key=lambda x: x['timestamp'], reverse=True)
            if len(mail_cache['emails']) > MAX_EMAILS:
                mail_cache['emails'] = mail_cache['emails'][:MAX_EMAILS]
            unread = sum(1 for e in mail_cache['emails'] if not e['is_read'])
            mail_cache['unread_count'] = unread
            mail_cache['last_poll'] = datetime.now().isoformat()
            save_cache()
            logging.info(f"新增 {len(new_emails)} 封，未读 {unread}，缓存 {len(mail_cache['emails'])} 封")
        else:
            mail_cache['last_poll'] = datetime.now().isoformat()
            save_cache()
    except Exception as e:
        logging.error(f"轮询失败: {e}")

def poll_loop():
    while True:
        poll_emails()
        time.sleep(POLL_INTERVAL)

# ★★★ 关键修复：在模块加载时立即启动轮询（gunicorn兼容）★★★
os.makedirs(CACHE_DIR, exist_ok=True)
load_cache()
poll_emails()
t = Thread(target=poll_loop, daemon=True)
t.start()
logging.info("服务启动，邮箱 %s，轮询间隔 %ds", HARD_CODED_EMAIL, POLL_INTERVAL)

@app.route('/api/emails')
def get_emails():
    emails = []
    for e in mail_cache.get('emails', []):
        emails.append({
            "uid": e['uid'], "subject": e['subject'], "sender": e['sender'],
            "date": e['date'], "timestamp": e['timestamp'], "body": e['body'],
            "is_read": e['is_read'], "is_unread_imap": e.get('is_unread_imap', False)
        })
    return jsonify({
        "emails": emails, "last_poll": mail_cache.get('last_poll'),
        "unread_count": mail_cache.get('unread_count', 0)
    })

@app.route('/api/emails/<uid>')
def get_email(uid):
    for e in mail_cache.get('emails', []):
        if e['uid'] == uid:
            return jsonify({
                "uid": e['uid'], "subject": e['subject'], "sender": e['sender'],
                "date": e['date'], "timestamp": e['timestamp'],
                "body": e['body'], "body_html": e.get('body_html', e['body']),
                "is_read": e['is_read'], "is_unread_imap": e.get('is_unread_imap', False)
            })
    return jsonify({"error": "not found"}), 404

@app.route('/api/unread-count')
def unread_count():
    return jsonify({
        "unread_count": mail_cache.get('unread_count', 0),
        "last_poll": mail_cache.get('last_poll')
    })

@app.route('/api/mark-read/<uid>', methods=['POST'])
def mark_read(uid):
    for e in mail_cache.get('emails', []):
        if e['uid'] == uid:
            e['is_read'] = True
            mail_cache['unread_count'] = sum(1 for em in mail_cache['emails'] if not em['is_read'])
            save_cache()
            return jsonify({"success": True})
    return jsonify({"error": "not found"}), 404

if __name__ == '__main__':
    logging.basicConfig(level=logging.INFO, format='%(asctime)s [%(levelname)s] %(message)s')
    port = int(os.getenv("PORT", "20044"))
    app.run(host='0.0.0.0', port=port, debug=False, threaded=True)
