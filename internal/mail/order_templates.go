package mail

import (
	"fmt"
	"strings"
	"time"
)

// Email transaksi pesanan (pembeli), kreator, dan admin. Gaya & tata letak
// sama dengan email akun di templates.go.

// OrderLine = satu baris pesanan; Price = harga satuan.
type OrderLine struct {
	Title   string
	Variant string // mis. "Putih, M"
	Qty     int
	Price   int
}

// OrderInfo = data pesanan yang dibutuhkan email. URL = halaman pesanan di web.
type OrderInfo struct {
	ID       string
	URL      string
	Items    []OrderLine
	Subtotal int
	Shipping int
	Discount int
	Total    int

	Courier       string    // kurir pengiriman (dipilih saat checkout, atau kurir resi)
	Tracking      string    // nomor resi
	Method        string    // metode pembayaran
	PayBefore     time.Time // batas bayar
	ConfirmWithin string    // mis. "48 jam": batas konfirmasi sebelum selesai otomatis
	Buyer         string    // nama pembeli (email ke admin)
}

// Rupiah memformat bilangan bulat sebagai "Rp1.234.000".
func Rupiah(n int) string {
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	var b strings.Builder
	if neg {
		b.WriteByte('-')
	}
	b.WriteString("Rp")
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(r)
	}
	return b.String()
}

var (
	wib     = time.FixedZone("WIB", 7*3600)
	bulanID = [...]string{"Januari", "Februari", "Maret", "April", "Mei", "Juni", "Juli", "Agustus", "September", "Oktober", "November", "Desember"}
	hariID  = [...]string{"Minggu", "Senin", "Selasa", "Rabu", "Kamis", "Jumat", "Sabtu"}
)

// wibTime: "Jumat, 27 November 2026 14.30 WIB".
func wibTime(t time.Time) string {
	t = t.In(wib)
	return fmt.Sprintf("%s, %d %s %d %02d.%02d WIB", hariID[t.Weekday()], t.Day(), bulanID[t.Month()-1], t.Year(), t.Hour(), t.Minute())
}

// orderLines = item + ongkir + diskon + total.
func orderLines(o OrderInfo) []line {
	lines := make([]line, 0, len(o.Items)+3)
	for _, it := range o.Items {
		label := fmt.Sprintf("%d× %s", it.Qty, it.Title)
		if it.Variant != "" {
			label += " (" + it.Variant + ")"
		}
		lines = append(lines, line{Label: label, Value: Rupiah(it.Price * it.Qty)})
	}
	ship := "Ongkos kirim"
	if o.Courier != "" {
		ship += " (" + o.Courier + ")"
	}
	lines = append(lines, line{Label: ship, Value: Rupiah(o.Shipping)})
	if o.Discount > 0 {
		lines = append(lines, line{Label: "Diskon", Value: "-" + Rupiah(o.Discount)})
	}
	lines = append(lines, line{Label: "Total", Value: Rupiah(o.Total), Bold: true})
	return lines
}

// ---- pembeli -----------------------------------------------------------------

func OrderCreated(to, name string, o OrderInfo) Message {
	return render(to, fmt.Sprintf("Pesanan %s menunggu pembayaran", o.ID), content{
		Name: name,
		Paragraphs: []string{
			fmt.Sprintf("Terima kasih sudah berbelanja di KaryaKita! Pesanan %s sudah kami terima. Berikut rinciannya:", o.ID),
		},
		Lines: orderLines(o),
		After: []string{
			fmt.Sprintf("Selesaikan pembayaran sebelum %s. Lewat dari batas itu, pesanan dibatalkan otomatis.", wibTime(o.PayBefore)),
		},
		ButtonURL: o.URL, ButtonLabel: "Bayar sekarang",
		Footer: "Tidak merasa memesan? Abaikan email ini — pesanan yang tidak dibayar akan dibatalkan otomatis.",
	})
}

func OrderPaid(to, name string, o OrderInfo) Message {
	paragraph := fmt.Sprintf("Pembayaran pesanan %s sebesar %s sudah kami terima. Pesananmu segera masuk antrean produksi.", o.ID, Rupiah(o.Total))
	if o.Method != "" {
		paragraph = fmt.Sprintf("Pembayaran pesanan %s sebesar %s lewat %s sudah kami terima. Pesananmu segera masuk antrean produksi.", o.ID, Rupiah(o.Total), o.Method)
	}
	return render(to, fmt.Sprintf("Pembayaran pesanan %s diterima", o.ID), content{
		Name:       name,
		Paragraphs: []string{paragraph},
		Lines:      orderLines(o),
		After:      []string{"Kami akan mengabari lagi lewat email saat paketmu dikirim."},
		ButtonURL:  o.URL, ButtonLabel: "Lihat pesanan",
		Footer: "Email ini dikirim otomatis setelah pembayaran terverifikasi.",
	})
}

func OrderShipped(to, name string, o OrderInfo) Message {
	return render(to, fmt.Sprintf("Pesanan %s sudah dikirim", o.ID), content{
		Name: name,
		Paragraphs: []string{
			fmt.Sprintf("Kabar baik! Pesanan %s sudah diserahkan ke kurir dan dalam perjalanan ke alamatmu.", o.ID),
		},
		Lines: []line{
			{Label: "Kurir", Value: o.Courier},
			{Label: "Nomor resi", Value: o.Tracking, Bold: true},
		},
		After:     []string{"Pantau perjalanan paket dengan nomor resi di atas lewat situs atau aplikasi kurir."},
		ButtonURL: o.URL, ButtonLabel: "Lihat pesanan",
		Footer: "Email ini dikirim otomatis saat pesananmu diserahkan ke kurir.",
	})
}

func OrderArrived(to, name string, o OrderInfo) Message {
	within := o.ConfirmWithin
	if within == "" {
		within = "48 jam"
	}
	return render(to, fmt.Sprintf("Pesanan %s sudah tiba — mohon konfirmasi", o.ID), content{
		Name: name,
		Paragraphs: []string{
			fmt.Sprintf("Menurut laporan kurir, pesanan %s sudah tiba di alamatmu.", o.ID),
			"Kalau paketnya sudah kamu terima dengan baik, tekan tombol \"Konfirmasi pesanan diterima\" di bawah.",
			fmt.Sprintf("Kalau tidak ada konfirmasi dalam %s, pesanan otomatis kami tandai selesai.", within),
		},
		ButtonURL: o.URL, ButtonLabel: "Konfirmasi pesanan diterima",
		Footer: "Ada masalah dengan paketnya? Balas email ini atau hubungi kami sebelum menekan konfirmasi.",
	})
}

func OrderCompleted(to, name string, o OrderInfo) Message {
	within := o.ConfirmWithin
	if within == "" {
		within = "48 jam"
	}
	return render(to, fmt.Sprintf("Pesanan %s selesai", o.ID), content{
		Name: name,
		Paragraphs: []string{
			fmt.Sprintf("Pesanan %s otomatis kami tandai selesai karena tidak ada konfirmasi dalam %s setelah paket tiba.", o.ID, within),
			"Terima kasih sudah berbelanja! Kalau kamu suka dengan produknya, tinggalkan ulasan untuk membantu kreatornya.",
		},
		ButtonURL: o.URL, ButtonLabel: "Lihat pesanan",
		Footer: "Email ini dikirim otomatis.",
	})
}

func OrderCancelled(to, name string, o OrderInfo) Message {
	return render(to, fmt.Sprintf("Pesanan %s dibatalkan", o.ID), content{
		Name: name,
		Paragraphs: []string{
			fmt.Sprintf("Pesanan %s dibatalkan otomatis karena pembayaran belum selesai sebelum batas waktu.", o.ID),
			"Kamu tidak dikenai biaya apa pun. Voucher yang dipakai (kalau ada) bisa dipakai lagi. Mau tetap membeli? Buat pesanan baru kapan saja.",
		},
		Lines:  orderLines(o),
		Footer: "Sudah terlanjur membayar? Hubungi kami dengan menyebut nomor pesanan di atas.",
	})
}

// ---- kreator -----------------------------------------------------------------

func DesignApproved(to, name, title, listingURL string) Message {
	return render(to, fmt.Sprintf("Desain \"%s\" disetujui dan sudah tayang", title), content{
		Name: name,
		Paragraphs: []string{
			fmt.Sprintf("Selamat! Desain \"%s\" sudah disetujui tim KaryaKita dan sekarang tayang di katalog.", title),
			"Setiap produk yang terjual menambah royalti ke saldomu.",
		},
		ButtonURL: listingURL, ButtonLabel: "Lihat produk",
		Footer: "Email ini dikirim otomatis saat pengajuan desainmu disetujui.",
	})
}

func DesignRejected(to, name, title, note string) Message {
	reason := "Admin tidak menyertakan catatan."
	if strings.TrimSpace(note) != "" {
		reason = "Catatan dari admin: " + strings.TrimSpace(note)
	}
	return render(to, fmt.Sprintf("Desain \"%s\" belum bisa disetujui", title), content{
		Name: name,
		Paragraphs: []string{
			fmt.Sprintf("Terima kasih sudah mengirim desain \"%s\". Sayangnya desain ini belum bisa kami setujui.", title),
			reason,
			"Kamu bisa memperbaiki desainnya lalu mengajukannya lagi dari Studio Mockup.",
		},
		Footer: "Email ini dikirim otomatis saat pengajuan desainmu ditinjau.",
	})
}

func PayoutPaid(to, name string, amount int, reference string) Message {
	return render(to, fmt.Sprintf("Penarikan saldo %s sudah ditransfer", Rupiah(amount)), content{
		Name: name,
		Paragraphs: []string{
			"Penarikan saldo royaltimu sudah kami transfer ke rekening yang terdaftar.",
		},
		Lines: []line{
			{Label: "Jumlah", Value: Rupiah(amount), Bold: true},
			{Label: "Nomor referensi transfer", Value: reference},
		},
		After:  []string{"Dana biasanya masuk dalam beberapa menit, paling lambat 1 hari kerja tergantung bank."},
		Footer: "Tidak merasa menarik saldo? Segera hubungi kami.",
	})
}

// ---- admin -------------------------------------------------------------------

func AdminNewPaidOrder(to string, o OrderInfo) Message {
	method := o.Method
	if method == "" {
		method = "-"
	}
	return render(to, fmt.Sprintf("[KaryaKita] Pesanan baru %s — %s", o.ID, Rupiah(o.Total)), content{
		Name: "Admin",
		Paragraphs: []string{
			fmt.Sprintf("Pesanan %s dari %s sudah dibayar (%s) dan siap masuk produksi.", o.ID, o.Buyer, method),
		},
		Lines:     orderLines(o),
		ButtonURL: o.URL, ButtonLabel: "Buka pesanan",
		Footer: "Notifikasi ini dikirim ke ADMIN_NOTIFY_EMAIL.",
	})
}
