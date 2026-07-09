# Mail Bridge - 邮件桥接服务

提供 REST API 接口，通过 IMAP 读取 163 邮箱邮件内容。

## API 接口

- `GET /api/emails` - 获取邮件列表
- `GET /api/emails/<uid>` - 获取邮件详情
- `GET /api/unread-count` - 获取未读数
- `POST /api/mark-read/<uid>` - 标记已读

## 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| MAIL_EMAIL | qq353324582@163.com | 邮箱账号 |
| MAIL_PASSWORD | - | 邮箱密码/授权码 |
| IMAP_SERVER | imap.163.com | IMAP 服务器 |
| IMAP_PORT | 993 | IMAP 端口 |
| POLL_INTERVAL | 60 | 轮询间隔(秒) |
| PORT | 20044 | HTTP 服务端口 |
