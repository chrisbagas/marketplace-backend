// Package mail mengirim email transaksional (verifikasi, reset password).
//
// Satu implementasi SMTP dipakai untuk dev dan produksi: di dev ia mengirim
// ke Mailpit (docker compose, kotak masuk di http://localhost:8025); di
// produksi ke SMTP penyedia email — Resend, Brevo, AWS SES, dsb. semuanya
// menyediakan SMTP, jadi ganti penyedia cukup lewat env var.
package mail

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

type Message struct {
	To      string
	Subject string
	Text    string // versi teks polos (wajib — sebagian klien email tidak merender HTML)
	HTML    string
}

type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// ---- SMTP -----------------------------------------------------------------

type SMTP struct {
	Host     string
	Port     string
	Username string // kosong = tanpa AUTH (Mailpit)
	Password string
	From     string // "KaryaKita <no-reply@karyakita.id>"
}

func (s SMTP) Send(ctx context.Context, m Message) error {
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("MAIL_FROM tidak valid: %w", err)
	}
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return fmt.Errorf("alamat tujuan tidak valid: %w", err)
	}
	raw, err := Build(s.From, m)
	if err != nil {
		return err
	}

	addr := net.JoinHostPort(s.Host, s.Port)
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	if s.Port == "465" { // TLS langsung (implicit TLS)
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: s.Host})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok && s.Port != "465" {
		if err := c.StartTLS(&tls.Config{ServerName: s.Host}); err != nil {
			return err
		}
	}
	if s.Username != "" {
		// PlainAuth menolak mengirim password lewat koneksi tanpa TLS (kecuali localhost)
		if err := c.Auth(smtp.PlainAuth("", s.Username, s.Password, s.Host)); err != nil {
			return err
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return err
	}
	if err := c.Rcpt(to.Address); err != nil {
		return err
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(raw); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// ---- Log (tanpa SMTP) -------------------------------------------------------

// Log hanya mencatat email ke log server. ShowBody=false di produksi supaya
// link berisi token tidak pernah tertulis ke log.
type Log struct{ ShowBody bool }

func (l Log) Send(_ context.Context, m Message) error {
	if l.ShowBody {
		log.Printf("[email] ke %s — %q\n%s", m.To, m.Subject, m.Text)
	} else {
		log.Printf("[email] TIDAK terkirim (SMTP belum dikonfigurasi): ke %s — %q", m.To, m.Subject)
	}
	return nil
}

// ---- MIME -----------------------------------------------------------------

// Build menyusun email multipart/alternative (teks + HTML) dalam UTF-8.
func Build(from string, m Message) ([]byte, error) {
	if strings.ContainsAny(m.To+m.Subject, "\r\n") {
		return nil, fmt.Errorf("header email tidak boleh berisi baris baru")
	}
	boundary := "kk-" + randomHex(12)
	domain := "karyakita.local"
	if addr, err := mail.ParseAddress(from); err == nil {
		if _, d, ok := strings.Cut(addr.Address, "@"); ok {
			domain = d
		}
	}

	var b bytes.Buffer
	header := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	header("From", from)
	header("To", m.To)
	header("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("Message-ID", fmt.Sprintf("<%s@%s>", randomHex(16), domain))
	header("MIME-Version", "1.0")
	header("Content-Type", fmt.Sprintf(`multipart/alternative; boundary="%s"`, boundary))
	b.WriteString("\r\n")

	part := func(contentType, body string) error {
		fmt.Fprintf(&b, "--%s\r\nContent-Type: %s; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n", boundary, contentType)
		qp := quotedprintable.NewWriter(&b)
		if _, err := qp.Write([]byte(body)); err != nil {
			return err
		}
		if err := qp.Close(); err != nil {
			return err
		}
		b.WriteString("\r\n")
		return nil
	}
	if err := part("text/plain", m.Text); err != nil {
		return nil, err
	}
	if m.HTML != "" {
		if err := part("text/html", m.HTML); err != nil {
			return nil, err
		}
	}
	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes(), nil
}

func randomHex(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}
