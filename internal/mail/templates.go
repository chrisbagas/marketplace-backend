package mail

import (
	"bytes"
	"fmt"
	"html/template"
)

// Template email transaksional (Bahasa Indonesia). Setiap email punya versi
// teks polos + HTML sederhana bergaya inline (klien email tidak memuat CSS luar).

var layout = template.Must(template.New("email").Parse(`<!doctype html>
<html lang="id"><body style="margin:0;background:#f7f3ea;font-family:Arial,Helvetica,sans-serif;color:#1d1d1b">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="padding:32px 12px"><tr><td align="center">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:480px;background:#ffffff;border-radius:16px;padding:32px">
<tr><td>
  <p style="margin:0 0 24px;font-size:20px;font-weight:800">Karya<span style="color:#0b6244">Kita</span></p>
  <p style="margin:0 0 16px;font-size:16px">Halo {{.Name}},</p>
  {{range .Paragraphs}}<p style="margin:0 0 16px;font-size:15px;line-height:1.6">{{.}}</p>{{end}}
  {{if .ButtonURL}}
  <p style="margin:24px 0"><a href="{{.ButtonURL}}" style="display:inline-block;background:#0b6244;color:#ffffff;text-decoration:none;font-weight:700;padding:12px 24px;border-radius:999px">{{.ButtonLabel}}</a></p>
  <p style="margin:0 0 16px;font-size:12px;color:#6b6b66;line-height:1.5">Tombol tidak berfungsi? Salin link ini ke browser:<br><a href="{{.ButtonURL}}" style="color:#0b6244;word-break:break-all">{{.ButtonURL}}</a></p>
  {{end}}
  <p style="margin:24px 0 0;font-size:12px;color:#6b6b66;line-height:1.5">{{.Footer}}</p>
</td></tr></table>
</td></tr></table>
</body></html>`))

type content struct {
	Name        string
	Paragraphs  []string
	ButtonURL   string
	ButtonLabel string
	Footer      string
}

func render(to, subject string, c content) Message {
	var html bytes.Buffer
	_ = layout.Execute(&html, c) // template statis — hanya gagal bila kodenya salah

	var text bytes.Buffer
	fmt.Fprintf(&text, "Halo %s,\n\n", c.Name)
	for _, p := range c.Paragraphs {
		fmt.Fprintf(&text, "%s\n\n", p)
	}
	if c.ButtonURL != "" {
		fmt.Fprintf(&text, "%s:\n%s\n\n", c.ButtonLabel, c.ButtonURL)
	}
	fmt.Fprintf(&text, "%s\n\n— KaryaKita\n", c.Footer)
	return Message{To: to, Subject: subject, Text: text.String(), HTML: html.String()}
}

func VerifyEmail(to, name, link string) Message {
	return render(to, "Verifikasi email akun KaryaKita kamu", content{
		Name: name,
		Paragraphs: []string{
			"Terima kasih sudah bergabung di KaryaKita! Satu langkah lagi: pastikan email ini benar-benar milikmu.",
			"Link berlaku 24 jam.",
		},
		ButtonURL: link, ButtonLabel: "Verifikasi email",
		Footer: "Tidak merasa mendaftar? Abaikan email ini — akunnya tidak akan aktif sepenuhnya tanpa verifikasi.",
	})
}

func ResetPassword(to, name, link string) Message {
	return render(to, "Atur ulang password KaryaKita", content{
		Name: name,
		Paragraphs: []string{
			"Kami menerima permintaan untuk mengatur ulang password akunmu.",
			"Link berlaku 30 menit dan hanya bisa dipakai sekali.",
		},
		ButtonURL: link, ButtonLabel: "Buat password baru",
		Footer: "Tidak merasa meminta? Abaikan email ini — password kamu tidak berubah.",
	})
}

func PasswordChanged(to, name, forgotLink string) Message {
	return render(to, "Password KaryaKita kamu baru saja diubah", content{
		Name: name,
		Paragraphs: []string{
			"Password akunmu baru saja diubah, dan semua perangkat lain telah dikeluarkan dari akun.",
			"Kalau ini bukan kamu, segera atur ulang password lewat tombol di bawah.",
		},
		ButtonURL: forgotLink, ButtonLabel: "Amankan akun",
		Footer: "Email ini dikirim otomatis demi keamanan akunmu.",
	})
}
