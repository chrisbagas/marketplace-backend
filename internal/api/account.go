package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"karyakita/api/internal/db"
	"karyakita/api/internal/mail"
)

// Pengaturan akun (halaman /akun): ganti password, ganti email, hapus akun.
// Semua endpoint di sini (kecuali konfirmasi email) dibungkus s.authed.
//
// Keputusan untuk akun Google:
//   - Akun tanpa password (hanya Google) tidak bisa ganti password/email di sini:
//     identitasnya adalah akun Google, dan tidak ada "password saat ini" untuk
//     dibuktikan. Mereka membuat password lewat "Lupa password" (link ke email
//     terverifikasi), setelah itu berlaku aturan akun biasa.
//   - Akun Google yang sudah punya password boleh ganti email; tautan Google
//     (google_sub) tetap berlaku, jadi login Google tidak terpengaruh.
//   - Hapus akun tanpa password dikonfirmasi lewat link sekali pakai (30 menit)
//     ke email akun, lalu tombol konfirmasi di halaman /akun.

const (
	purposeChangeEmail   = "change_email"
	purposeDeleteAccount = "delete_account"
	deleteConfirmTTL     = 30 * time.Minute
	deletedUserName      = "Pengguna dihapus"
)

// currentSessionID: id (sha256) sesi yang dipakai request ini.
func currentSessionID(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return hashToken(c.Value)
}

// checkPassword membandingkan password dengan hash milik user (bukan dari sesi,
// karena hash tidak ikut di struct User).
func (s *Server) checkPassword(ctx context.Context, userID, password string) (bool, error) {
	var hash *string
	if err := s.pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE id = $1`, userID).Scan(&hash); err != nil {
		return false, err
	}
	if hash == nil {
		return false, nil
	}
	return bcrypt.CompareHashAndPassword([]byte(*hash), []byte(password)) == nil, nil
}

const tooManyAccountAttempts = "Terlalu banyak percobaan. Coba lagi nanti."

// POST /api/account/password {currentPassword, newPassword}
// Password saat ini wajib. Semua sesi LAIN dicabut; sesi ini tetap masuk.
func (s *Server) postChangePassword(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	if !u.HasPassword {
		errJSON(w, http.StatusBadRequest, "Akun ini masuk lewat Google dan belum punya password. Buat password lewat \"Lupa password\".")
		return
	}
	if !s.limiter.allow("pw-change:"+u.ID, 5, 15*time.Minute) {
		errJSON(w, http.StatusTooManyRequests, tooManyAccountAttempts)
		return
	}
	if msg := passwordProblem(body.NewPassword); msg != "" {
		errJSON(w, http.StatusBadRequest, msg)
		return
	}
	ctx := r.Context()
	ok, err := s.checkPassword(ctx, u.ID, body.CurrentPassword)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		errJSON(w, http.StatusForbidden, "Password saat ini salah")
		return
	}
	if body.NewPassword == body.CurrentPassword {
		errJSON(w, http.StatusBadRequest, "Password baru harus berbeda dari password saat ini")
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(body.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, u.ID, string(hash)); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// perangkat lain keluar, sesi ini tetap; link reset yang masih beredar dimatikan
	if _, err := tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1 AND id <> $2`, u.ID, currentSessionID(r)); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if _, err := tx.Exec(ctx,
		`DELETE FROM auth_tokens WHERE user_id = $1 AND purpose = $2 AND used_at IS NULL`, u.ID, purposeReset); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.sendMail(mail.PasswordChanged(u.Email, u.Name, s.auth.AppURL+"/lupa-password"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// POST /api/account/email {newEmail, password}
// Mengirim link konfirmasi ke alamat BARU. Email akun baru berganti saat link
// dikonfirmasi (postConfirmEmailChange).
func (s *Server) postChangeEmail(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		NewEmail string `json:"newEmail"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	if !u.HasPassword {
		errJSON(w, http.StatusBadRequest, "Akun ini masuk lewat Google dan belum punya password. Buat password lewat \"Lupa password\" dulu untuk mengganti email.")
		return
	}
	if !s.limiter.allow("email-change:"+u.ID, 10, time.Hour) {
		errJSON(w, http.StatusTooManyRequests, tooManyAccountAttempts)
		return
	}
	newEmail := strings.ToLower(strings.TrimSpace(body.NewEmail))
	if !validEmail(newEmail) {
		errJSON(w, http.StatusBadRequest, "Format email tidak valid")
		return
	}
	if newEmail == strings.ToLower(u.Email) {
		errJSON(w, http.StatusBadRequest, "Itu sudah menjadi email akunmu")
		return
	}
	ctx := r.Context()
	ok, err := s.checkPassword(ctx, u.ID, body.Password)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !ok {
		errJSON(w, http.StatusForbidden, "Password salah")
		return
	}
	var taken bool
	if err := s.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM users WHERE lower(email) = $1)`, newEmail).Scan(&taken); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if taken {
		errJSON(w, http.StatusConflict, "Email itu sudah dipakai akun lain")
		return
	}
	token, err := s.newAuthTokenPayload(ctx, u.ID, purposeChangeEmail, verifyTTL, newEmail)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.sendMail(mail.ChangeEmailVerify(newEmail, u.Name, s.link("/konfirmasi-email", token)))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sentTo": newEmail})
}

// POST /api/account/email/confirm {token} — publik: link dibuka dari email di
// browser mana pun; token yang dikirim ke alamat baru itulah buktinya.
func (s *Server) postConfirmEmailChange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := readJSON(r, &body); err != nil || body.Token == "" {
		errJSON(w, http.StatusBadRequest, "Link konfirmasi tidak valid")
		return
	}
	if !s.limiter.allow("email-confirm-ip:"+clientIP(r), 30, time.Hour) {
		errJSON(w, http.StatusTooManyRequests, tooManyAccountAttempts)
		return
	}
	ctx := r.Context()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)
	userID, newEmail, err := consumeAuthTokenPayload(ctx, tx, body.Token, purposeChangeEmail)
	if errors.Is(err, errBadToken) || (err == nil && newEmail == "") {
		errJSON(w, http.StatusBadRequest, "Link tidak valid, sudah dipakai, atau kedaluwarsa — minta link baru dari halaman Akun")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	var oldEmail, name string
	err = tx.QueryRow(ctx,
		`SELECT email, name FROM users WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, userID).Scan(&oldEmail, &name)
	if errors.Is(err, pgx.ErrNoRows) {
		errJSON(w, http.StatusBadRequest, "Link tidak valid")
		return
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// kepemilikan alamat baru terbukti lewat link → terverifikasi. Unik per email:
	// bila sementara itu dipakai akun lain, UPDATE gagal dan token ikut batal terpakai (rollback).
	if _, err := tx.Exec(ctx, `UPDATE users SET email = $2, email_verified = true WHERE id = $1`, userID, newEmail); err != nil {
		if msg := uniqueViolationMessage(err); msg != "" {
			errJSON(w, http.StatusConflict, "Email itu sudah dipakai akun lain")
			return
		}
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// link verifikasi/reset yang dikirim ke alamat lama tidak boleh berlaku lagi
	if _, err := tx.Exec(ctx,
		`DELETE FROM auth_tokens WHERE user_id = $1 AND purpose IN ($2,$3) AND used_at IS NULL`,
		userID, purposeVerify, purposeReset); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.sendMail(mail.EmailChangedNotice(oldEmail, name, maskEmail(newEmail), s.auth.AppURL+"/lupa-password"))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "email": newEmail})
}

// maskEmail: "budi@gmail.com" → "b***@gmail.com".
func maskEmail(e string) string {
	local, domain, ok := strings.Cut(e, "@")
	if !ok || local == "" {
		return e
	}
	return local[:1] + "***@" + domain
}

// ---- hapus akun -------------------------------------------------------------

// deletionBlockers: alasan akun belum boleh dihapus (kosong = boleh). Dipanggil
// juga di dalam transaksi penghapusan, supaya hasilnya tidak basi.
func deletionBlockers(ctx context.Context, q querier, u *User) ([]string, error) {
	var blockers []string
	if u.Role == "admin" {
		blockers = append(blockers, "Akun admin tidak bisa dihapus lewat halaman ini")
	}
	var open int
	if err := q.QueryRow(ctx, `
		SELECT count(*) FROM orders WHERE user_id = $1 AND status::text NOT IN ('selesai', 'dibatalkan')`, u.ID).Scan(&open); err != nil {
		return nil, err
	}
	if open > 0 {
		blockers = append(blockers, fmt.Sprintf("Masih ada %d pesanan yang belum selesai. Tunggu sampai selesai atau dibatalkan.", open))
	}
	if u.Designer != nil {
		bal, err := designerBalance(ctx, q, u.Designer.ID)
		if err != nil {
			return nil, err
		}
		if bal.InProcess > 0 {
			blockers = append(blockers, "Masih ada penarikan saldo yang belum selesai diproses.")
		}
		if bal.Pending > 0 {
			blockers = append(blockers, "Masih ada pesanan produkmu yang belum selesai, jadi royaltimu masih tertahan.")
		}
		if bal.Available >= PayoutMinimum {
			blockers = append(blockers, "Saldo royaltimu belum ditarik. Tarik dulu di Studio Kreator.")
		}
	}
	return blockers, nil
}

// GET /api/account/deletion → {blockers, hasPassword}
func (s *Server) getDeletionCheck(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	blockers, err := deletionBlockers(r.Context(), s.pool, u)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if blockers == nil {
		blockers = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"blockers": blockers, "hasPassword": u.HasPassword})
}

// POST /api/account/delete/request — akun tanpa password: kirim link konfirmasi ke email akun.
func (s *Server) postDeleteRequest(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	if u.HasPassword {
		errJSON(w, http.StatusBadRequest, "Akun ini punya password — konfirmasi penghapusan dengan password")
		return
	}
	if !s.limiter.allow("account-delete:"+u.ID, 10, time.Hour) {
		errJSON(w, http.StatusTooManyRequests, tooManyAccountAttempts)
		return
	}
	blockers, err := deletionBlockers(r.Context(), s.pool, u)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(blockers) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": blockers[0], "blockers": blockers})
		return
	}
	token, err := s.newAuthTokenPayload(r.Context(), u.ID, purposeDeleteAccount, deleteConfirmTTL, "")
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.sendMail(mail.DeleteAccountConfirm(u.Email, u.Name, s.link("/akun", token)))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "sentTo": u.Email})
}

// POST /api/account/delete {password} atau {token} (akun tanpa password)
// Menganonimkan akun, bukan menghapus barisnya. Lihat anonymizeUser.
func (s *Server) postDeleteAccount(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r)
	var body struct {
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	if err := readJSON(r, &body); err != nil {
		errJSON(w, http.StatusBadRequest, "Bad request")
		return
	}
	if !s.limiter.allow("account-delete:"+u.ID, 10, time.Hour) {
		errJSON(w, http.StatusTooManyRequests, tooManyAccountAttempts)
		return
	}
	ctx := r.Context()
	if u.HasPassword {
		ok, err := s.checkPassword(ctx, u.ID, body.Password)
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			errJSON(w, http.StatusForbidden, "Password salah")
			return
		}
	} else if body.Token == "" {
		errJSON(w, http.StatusBadRequest, "Minta link konfirmasi ke emailmu dulu")
		return
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer tx.Rollback(ctx)

	// Kunci baris user (pesanan baru memegang FK ke baris ini, jadi tidak bisa menyelip)
	// dan baris kreator (permintaan penarikan juga mengunci baris ini).
	var locked string
	if err := tx.QueryRow(ctx, `SELECT id FROM users WHERE id = $1 AND deleted_at IS NULL FOR UPDATE`, u.ID).Scan(&locked); err != nil {
		errJSON(w, http.StatusUnauthorized, "Silakan masuk terlebih dahulu")
		return
	}
	if u.Designer != nil {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM designers WHERE id = $1 FOR UPDATE`, u.Designer.ID); err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if !u.HasPassword {
		// dikonsumsi di transaksi yang sama: bila ada penghalang, token ikut kembali (rollback)
		owner, _, err := consumeAuthTokenPayload(ctx, tx, body.Token, purposeDeleteAccount)
		if errors.Is(err, errBadToken) || (err == nil && owner != u.ID) {
			errJSON(w, http.StatusBadRequest, "Link konfirmasi tidak valid, sudah dipakai, atau kedaluwarsa — minta link baru")
			return
		}
		if err != nil {
			errJSON(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	blockers, err := deletionBlockers(ctx, tx, u)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(blockers) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"error": blockers[0], "blockers": blockers})
		return
	}
	listings, err := anonymizeUser(ctx, tx, u)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := tx.Commit(ctx); err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(listings) > 0 {
		s.reindexSearch(ctx, listings...) // listing nonaktif keluar dari pencarian
	}
	s.sendMail(mail.AccountDeleted(u.Email, u.Name))
	s.clearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// anonymizeUser menggantikan data pribadi dengan tombstone di dalam tx pemanggil
// dan mengembalikan id listing yang dinonaktifkan. Yang dipertahankan:
//   - baris users (deleted_at terisi; email/username tombstone yang unik per id);
//   - pesanan + item (snapshot judul/harga/ukuran) + pembayaran — pembukuan;
//     identitas pembeli di pesanan (nama, email, telepon, alamat, catatan) dikosongkan
//     karena pesanan yang sampai sini sudah final;
//   - desain, listing (dinonaktifkan), royalti, riwayat penarikan, dan baris kreator
//     (nama/bio/rekening dikosongkan) supaya riwayat keuangan tetap utuh.
//
// File yang pernah diunggah (/uploads, termasuk foto profil) tidak dihapus di sini.
func anonymizeUser(ctx context.Context, tx pgx.Tx, u *User) ([]string, error) {
	steps := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM sessions WHERE user_id = $1`, []any{u.ID}},
		{`DELETE FROM auth_tokens WHERE user_id = $1`, []any{u.ID}},
		{`DELETE FROM user_designs WHERE user_id = $1`, []any{u.ID}},
		{`UPDATE orders SET cust_name = $2, cust_email = '', cust_phone = '', cust_address = '',
			cust_city = '', cust_postal = '', notes = '' WHERE user_id = $1`, []any{u.ID, deletedUserName}},
		{`UPDATE reviews SET author = $2 WHERE order_id IN (SELECT id FROM orders WHERE user_id = $1)`, []any{u.ID, deletedUserName}},
		{`UPDATE users SET
			email = $2, username = $3, name = $4,
			phone = '', address = '', city = '', postal_code = '',
			preferred_payment = '', preferred_courier = '', settings = '{}',
			password_hash = NULL, google_sub = NULL, email_verified = false,
			last_login_at = NULL, deleted_at = now()
		  WHERE id = $1`, []any{u.ID, "deleted+" + u.ID + "@deleted.invalid", "deleted-" + u.ID, deletedUserName}},
	}
	for _, st := range steps {
		if _, err := tx.Exec(ctx, st.sql, st.args...); err != nil {
			return nil, err
		}
	}
	if u.Designer == nil {
		return nil, nil
	}

	rows, err := tx.Query(ctx, `
		UPDATE listings SET active = false
		WHERE active AND design_id IN (SELECT id FROM designs WHERE designer_id = $1)
		RETURNING id`, u.Designer.ID)
	if err != nil {
		return nil, err
	}
	listings, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	// pengajuan yang menunggu tidak boleh disetujui setelah kreatornya pergi
	if _, err := tx.Exec(ctx,
		`UPDATE design_submissions SET status = 'ditolak', note = 'Akun kreator dihapus' WHERE designer_id = $1 AND status = 'review'`,
		u.Designer.ID); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE design_submissions SET designer_name = $2 WHERE designer_id = $1`, u.Designer.ID, deletedUserName); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE designers SET name = $2, bio = '', city = '', avatar_uri = $3,
		       bank_name = NULL, bank_account = NULL, bank_holder = NULL
		WHERE id = $1`, u.Designer.ID, deletedUserName, db.AvatarURI(u.Designer.Hue, "?")); err != nil {
		return nil, err
	}
	return listings, nil
}
