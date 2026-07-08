package webapp

import (
	"fmt"
	"net/smtp"
	"strings"
)

// Mailer sends plain-text mail over SMTP — the lowest-common-denominator
// transport a self-hoster can point at anything (Gmail app password, SES,
// Mailgun, a local relay). No SDK, stdlib only. A nil Mailer reports itself
// as unconfigured so callers can fall back to logging the message.
type Mailer struct {
	Host string // e.g. smtp.gmail.com
	Port int    // e.g. 587 (STARTTLS)
	User string
	Pass string
	From string // e.g. drive@example.com
}

func (m *Mailer) Send(to, subject, body string) error {
	if m == nil || m.Host == "" {
		return fmt.Errorf("no smtp configured")
	}
	from := m.From
	if from == "" {
		from = m.User
	}
	msg := strings.Join([]string{
		"From: " + from,
		"To: " + to,
		"Subject: " + subject,
		"MIME-Version: 1.0",
		"Content-Type: text/plain; charset=utf-8",
		"",
		body,
	}, "\r\n")
	addr := fmt.Sprintf("%s:%d", m.Host, m.Port)
	var auth smtp.Auth
	if m.User != "" {
		auth = smtp.PlainAuth("", m.User, m.Pass, m.Host)
	}
	// smtp.SendMail negotiates STARTTLS when the server offers it.
	return smtp.SendMail(addr, auth, from, []string{to}, []byte(msg))
}
