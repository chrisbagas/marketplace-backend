package mail

import (
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

func TestBuildMultipartUTF8(t *testing.T) {
	m := ResetPassword("budi@mail.com", "Budi <script>", "http://localhost:3000/reset-password?token=abc_DEF-123")
	raw, err := Build("KaryaKita <no-reply@karyakita.id>", m)
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("bukan email RFC 5322 yang valid: %v", err)
	}
	subject, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if subject != m.Subject {
		t.Errorf("subject %q, want %q", subject, m.Subject)
	}
	_, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	parts := map[string]string{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(p) // NextPart sudah mendekode quoted-printable
		ct, _, _ := mime.ParseMediaType(p.Header.Get("Content-Type"))
		parts[ct] = string(body)
	}
	if !strings.Contains(parts["text/plain"], "token=abc_DEF-123") {
		t.Error("versi teks tidak memuat link reset")
	}
	if !strings.Contains(parts["text/html"], `href="http://localhost:3000/reset-password?token=abc_DEF-123"`) {
		t.Error("versi HTML tidak memuat link reset")
	}
	if strings.Contains(parts["text/html"], "<script>") {
		t.Error("nama pengguna tidak di-escape di HTML")
	}
}

func TestBuildRejectsHeaderInjection(t *testing.T) {
	if _, err := Build("a@b.co", Message{To: "x@y.co\r\nBcc: korban@z.co", Subject: "s", Text: "t"}); err == nil {
		t.Fatal("baris baru di header diterima")
	}
}
