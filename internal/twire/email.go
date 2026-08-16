package twire

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

const (
	googleSMTPHost = "smtp.gmail.com"
	googleSMTPAddr = "smtp.gmail.com:587"

	// smtpDialTimeout bounds the TCP connect, and smtpTotalTimeout the whole
	// conversation after it. net/smtp's SendMail sets no deadline at all, so
	// a relay that accepts the connection and then stops responding blocks
	// its caller forever. That matters here because alerts are sent from a
	// goroutine spawned per tripped canary: a blackholed SMTP server would
	// strand one goroutine and one socket per alert, indefinitely.
	smtpDialTimeout  = 10 * time.Second
	smtpTotalTimeout = 30 * time.Second
)

// sendEmail delivers a plaintext message to cfg's recipients via Gmail's
// SMTP relay (smtp.gmail.com:587, STARTTLS). Use a Google App Password for
// cfg.SMTPPassword — account passwords are rejected by Google when
// "less secure app access" is disabled, which it is by default.
//
// This is net/smtp's SendMail flow written out so a deadline can be set on
// the connection; SendMail itself offers no way to bound how long it waits.
func sendEmail(cfg AlertConfig, subject, body string) error {
	// Preserved from SendMail, which refuses addresses containing a newline:
	// without it an address could inject extra SMTP commands or headers.
	for _, addr := range append([]string{cfg.From}, cfg.Recipients...) {
		if strings.ContainsAny(addr, "\r\n") {
			return fmt.Errorf("twire: send email: address %q contains a newline", addr)
		}
	}

	conn, err := net.DialTimeout("tcp", googleSMTPAddr, smtpDialTimeout)
	if err != nil {
		return fmt.Errorf("twire: send email: dial: %w", err)
	}
	// One deadline covering the whole exchange, so no individual step can
	// hang. Set before the client is built so even the greeting is bounded.
	if err := conn.SetDeadline(time.Now().Add(smtpTotalTimeout)); err != nil {
		conn.Close()
		return fmt.Errorf("twire: send email: set deadline: %w", err)
	}

	c, err := smtp.NewClient(conn, googleSMTPHost)
	if err != nil {
		conn.Close()
		return fmt.Errorf("twire: send email: greeting: %w", err)
	}
	// Close tears down the connection on every path, including the error
	// returns below, which Quit alone would not do.
	defer c.Close()

	if err := c.StartTLS(&tls.Config{ServerName: googleSMTPHost}); err != nil {
		return fmt.Errorf("twire: send email: starttls: %w", err)
	}
	if err := c.Auth(smtp.PlainAuth("", cfg.SMTPUsername, cfg.SMTPPassword, googleSMTPHost)); err != nil {
		return fmt.Errorf("twire: send email: auth: %w", err)
	}
	if err := c.Mail(cfg.From); err != nil {
		return fmt.Errorf("twire: send email: from: %w", err)
	}
	for _, rcpt := range cfg.Recipients {
		if err := c.Rcpt(rcpt); err != nil {
			return fmt.Errorf("twire: send email: recipient %q: %w", rcpt, err)
		}
	}

	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("twire: send email: data: %w", err)
	}
	if _, err := w.Write(buildMessage(cfg.From, cfg.Recipients, subject, body)); err != nil {
		return fmt.Errorf("twire: send email: write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("twire: send email: finish message: %w", err)
	}
	if err := c.Quit(); err != nil {
		return fmt.Errorf("twire: send email: quit: %w", err)
	}
	return nil
}

// buildMessage assembles a minimal RFC 5322 plaintext email.
func buildMessage(from string, to []string, subject, body string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(body)
	return []byte(b.String())
}
