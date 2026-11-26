package api

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"karyakita/api/internal/mail"
)

// Email transaksi pesanan, kreator, dan admin.
//
// Aturan: semua fungsi di sini dipanggil SETELAH perubahan status ter-commit
// (tidak pernah di dalam transaksi yang masih bisa rollback), dan hanya oleh
// jalur yang benar-benar mengubah status — jalur yang dijaga kunci baris +
// cek status, jadi webhook ulang / klik ganda tidak mengirim dua kali.
// Pengiriman memakai s.sendMail (latar belakang); kegagalan hanya dicatat di
// log dan tidak pernah menggagalkan perubahan status yang sudah tersimpan.

// notifyCtx lepas dari pembatalan request (klien putus setelah commit tidak
// boleh membuat email hilang) tapi tetap dibatasi waktu.
func notifyCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
}

// orderMail memuat pesanan untuk dijadikan email. to = alamat pembeli:
// email di data pesanan bila ada, selain itu email akun pemesan.
func (s *Server) orderMail(ctx context.Context, orderID string) (info mail.OrderInfo, to, name string, err error) {
	o, err := s.loadOrder(ctx, orderID)
	if err != nil {
		return info, "", "", err
	}
	to, name = strings.TrimSpace(o.Customer.Email), o.Customer.Name
	if to == "" && o.UserID != "" {
		if err := s.pool.QueryRow(ctx, `SELECT email FROM users WHERE id=$1`, o.UserID).Scan(&to); err != nil {
			return info, "", "", err
		}
	}
	info = mail.OrderInfo{
		ID:            o.ID,
		URL:           s.auth.AppURL + "/order/" + url.PathEscape(o.ID),
		Subtotal:      o.Subtotal,
		Shipping:      o.Shipping.Cost,
		Discount:      o.Discount,
		Total:         o.Total,
		Courier:       o.Shipping.Courier,
		PayBefore:     time.UnixMilli(o.CreatedAt).Add(UnpaidOrderTTL),
		ConfirmWithin: fmt.Sprintf("%d jam", int(AutoCompleteAfter.Hours())),
		Buyer:         o.Customer.Name,
	}
	if o.Payment.Method != "" && o.Payment.Method != "belum dipilih" {
		info.Method = o.Payment.Method
	}
	if o.Shipment != nil {
		info.Courier, info.Tracking = o.Shipment.Courier, o.Shipment.TrackingNumber
	}
	for _, it := range o.Items {
		variant := make([]string, 0, 2)
		for _, v := range []string{it.Color, it.Size} {
			if v = strings.TrimSpace(v); v != "" {
				variant = append(variant, strings.ToUpper(v[:1])+v[1:])
			}
		}
		info.Items = append(info.Items, mail.OrderLine{
			Title: it.Title, Variant: strings.Join(variant, ", "), Qty: it.Qty, Price: it.Price,
		})
	}
	return info, to, name, nil
}

// mailBuyer mengirim email pesanan ke pembeli; build menyusun isinya.
func (s *Server) mailBuyer(ctx context.Context, orderID string, build func(to, name string, o mail.OrderInfo) mail.Message) {
	ctx, cancel := notifyCtx(ctx)
	defer cancel()
	info, to, name, err := s.orderMail(ctx, orderID)
	if err != nil {
		log.Printf("email pesanan %s: %v", orderID, err)
		return
	}
	if to == "" {
		return // pesanan lama tanpa email & tanpa akun
	}
	s.sendMail(build(to, name, info))
}

// mailOrderPaid: email "pembayaran diterima" ke pembeli + pemberitahuan ke
// ADMIN_NOTIFY_EMAIL (dilewati diam-diam bila kosong).
func (s *Server) mailOrderPaid(ctx context.Context, orderID string) {
	ctx, cancel := notifyCtx(ctx)
	defer cancel()
	info, to, name, err := s.orderMail(ctx, orderID)
	if err != nil {
		log.Printf("email pesanan %s: %v", orderID, err)
		return
	}
	if to != "" {
		s.sendMail(mail.OrderPaid(to, name, info))
	}
	if admin := strings.TrimSpace(s.auth.AdminNotifyEmail); admin != "" {
		info.URL = s.auth.AppURL + "/admin/pesanan/" + url.PathEscape(orderID)
		s.sendMail(mail.AdminNewPaidOrder(admin, info))
	}
}

// designerContact: email + nama akun pemilik toko kreator. ok=false bila
// kreator tidak tertaut ke akun.
func (s *Server) designerContact(ctx context.Context, designerID string) (email, name string, ok bool) {
	err := s.pool.QueryRow(ctx, `
		SELECT u.email, u.name FROM designers d JOIN users u ON u.id = d.user_id WHERE d.id = $1`, designerID).
		Scan(&email, &name)
	if err != nil || email == "" {
		return "", "", false
	}
	return email, name, true
}

// mailDesignReviewed mengabari kreator hasil moderasi desain.
func (s *Server) mailDesignReviewed(ctx context.Context, subID, status, listingID string) {
	ctx, cancel := notifyCtx(ctx)
	defer cancel()
	var designerID *string
	var title string
	var note *string
	if err := s.pool.QueryRow(ctx, `SELECT designer_id, title, note FROM design_submissions WHERE id=$1`, subID).
		Scan(&designerID, &title, &note); err != nil {
		log.Printf("email desain %s: %v", subID, err)
		return
	}
	if designerID == nil {
		return
	}
	to, name, ok := s.designerContact(ctx, *designerID)
	if !ok {
		return
	}
	if status == "disetujui" {
		s.sendMail(mail.DesignApproved(to, name, title, s.auth.AppURL+"/product/"+url.PathEscape(listingID)))
		return
	}
	n := ""
	if note != nil {
		n = *note
	}
	s.sendMail(mail.DesignRejected(to, name, title, n))
}

// mailPayoutPaid mengabari kreator bahwa penarikan saldonya sudah ditransfer.
func (s *Server) mailPayoutPaid(ctx context.Context, designerID string, amount int, reference string) {
	ctx, cancel := notifyCtx(ctx)
	defer cancel()
	if to, name, ok := s.designerContact(ctx, designerID); ok {
		s.sendMail(mail.PayoutPaid(to, name, amount, reference))
	}
}
