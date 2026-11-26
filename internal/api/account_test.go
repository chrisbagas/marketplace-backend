package api

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"karyakita/api/internal/mail"
)

// Tes integrasi pengaturan akun (butuh TEST_DATABASE_URL, lihat google_test.go).

const accountPassword = "rahasia123"

// signupAs mendaftar lewat API (password accountPassword) dan mengosongkan email verifikasi.
func signupAs(t *testing.T, s *Server, fm *fakeMailer, username string) *http.Cookie {
	t.Helper()
	rec, out := call(s, "POST", "/api/auth/signup", map[string]any{
		"username": username, "email": username + "@mail.test", "password": accountPassword, "name": "Tes Akun",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("signup: %d %v", rec.Code, out)
	}
	fm.next(t) // email verifikasi
	return sessionCookieFrom(rec)
}

func loginAs(s *Server, identifier, password string) (int, *http.Cookie) {
	rec, _ := call(s, "POST", "/api/auth/login", map[string]any{"identifier": identifier, "password": password})
	if rec.Code != http.StatusOK {
		return rec.Code, nil
	}
	return rec.Code, sessionCookieFrom(rec)
}

func loggedIn(s *Server, c *http.Cookie) bool {
	_, out := call(s, "GET", "/api/auth/me", nil, c)
	return out["user"] != nil
}

func TestChangePasswordRevokesOtherSessions(t *testing.T) {
	s, fm, pool := emailServer(t)
	username := "tesakun_pw_" + randomHex(3)
	cleanupUser(t, pool, username)
	here := signupAs(t, s, fm, username)
	code, other := loginAs(s, username, accountPassword)
	if code != http.StatusOK {
		t.Fatalf("login kedua: %d", code)
	}

	// password saat ini salah → ditolak, tidak ada yang berubah
	rec, out := call(s, "POST", "/api/account/password", map[string]any{"currentPassword": "salah-banget", "newPassword": "passwordbaru1"}, here)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("password salah: %d %v, want 403", rec.Code, out)
	}
	if !loggedIn(s, other) {
		t.Fatal("percobaan gagal tidak boleh mencabut sesi lain")
	}
	if c, _ := loginAs(s, username, accountPassword); c != http.StatusOK {
		t.Fatalf("password lama harus tetap berlaku: %d", c)
	}
	fm.none(t)

	// tanpa login, password baru kependekan, password sama
	if rec, _ := call(s, "POST", "/api/account/password", map[string]any{"currentPassword": accountPassword, "newPassword": "passwordbaru1"}); rec.Code != http.StatusUnauthorized {
		t.Errorf("tanpa login: %d, want 401", rec.Code)
	}
	if rec, _ := call(s, "POST", "/api/account/password", map[string]any{"currentPassword": accountPassword, "newPassword": "pendek"}, here); rec.Code != http.StatusBadRequest {
		t.Errorf("password pendek: %d, want 400", rec.Code)
	}
	if rec, _ := call(s, "POST", "/api/account/password", map[string]any{"currentPassword": accountPassword, "newPassword": accountPassword}, here); rec.Code != http.StatusBadRequest {
		t.Errorf("password sama: %d, want 400", rec.Code)
	}

	rec, out = call(s, "POST", "/api/account/password", map[string]any{"currentPassword": accountPassword, "newPassword": "passwordbaru1"}, here)
	if rec.Code != http.StatusOK {
		t.Fatalf("ganti password: %d %v", rec.Code, out)
	}
	if m := fm.next(t); !strings.Contains(m.Subject, "diubah") || m.To != username+"@mail.test" {
		t.Errorf("email pemberitahuan: %+v", m)
	}
	if !loggedIn(s, here) {
		t.Error("sesi yang dipakai harus tetap masuk")
	}
	if loggedIn(s, other) {
		t.Error("sesi lain harus dicabut")
	}
	if c, _ := loginAs(s, username, accountPassword); c != http.StatusUnauthorized {
		t.Errorf("login dengan password lama: %d, want 401", c)
	}
	if c, _ := loginAs(s, username, "passwordbaru1"); c != http.StatusOK {
		t.Errorf("login dengan password baru: %d, want 200", c)
	}
}

func TestChangePasswordRateLimited(t *testing.T) {
	s, fm, pool := emailServer(t)
	username := "tesakun_rl_" + randomHex(3)
	cleanupUser(t, pool, username)
	here := signupAs(t, s, fm, username)
	for i := range 5 {
		if rec, _ := call(s, "POST", "/api/account/password", map[string]any{"currentPassword": "salah", "newPassword": "passwordbaru1"}, here); rec.Code != http.StatusForbidden {
			t.Fatalf("percobaan %d: %d, want 403", i+1, rec.Code)
		}
	}
	if rec, _ := call(s, "POST", "/api/account/password", map[string]any{"currentPassword": accountPassword, "newPassword": "passwordbaru1"}, here); rec.Code != http.StatusTooManyRequests {
		t.Errorf("percobaan ke-6: %d, want 429", rec.Code)
	}
}

func TestChangeEmailSwitchesOnlyAfterConfirmation(t *testing.T) {
	s, fm, pool := emailServer(t)
	tag := randomHex(3)
	username, otherName := "tesakun_em_"+tag, "tesakun_em2_"+tag
	cleanupUser(t, pool, username)
	cleanupUser(t, pool, otherName)
	here := signupAs(t, s, fm, username)
	signupAs(t, s, fm, otherName)
	oldEmail, newEmail := username+"@mail.test", username+"-baru@mail.test"

	for name, tc := range map[string]struct {
		body map[string]any
		want int
	}{
		"password salah":    {map[string]any{"newEmail": newEmail, "password": "salah"}, http.StatusForbidden},
		"format":            {map[string]any{"newEmail": "bukan-email", "password": accountPassword}, http.StatusBadRequest},
		"email sama":        {map[string]any{"newEmail": strings.ToUpper(oldEmail), "password": accountPassword}, http.StatusBadRequest},
		"dipakai akun lain": {map[string]any{"newEmail": otherName + "@MAIL.test", "password": accountPassword}, http.StatusConflict},
	} {
		if rec, out := call(s, "POST", "/api/account/email", tc.body, here); rec.Code != tc.want {
			t.Errorf("%s: %d %v, want %d", name, rec.Code, out, tc.want)
		}
	}
	fm.none(t)

	// permintaan pertama, lalu kedua: hanya link terbaru yang berlaku
	if rec, out := call(s, "POST", "/api/account/email", map[string]any{"newEmail": newEmail, "password": accountPassword}, here); rec.Code != http.StatusOK {
		t.Fatalf("minta ganti email: %d %v", rec.Code, out)
	}
	m1 := fm.next(t)
	if m1.To != newEmail {
		t.Fatalf("link konfirmasi dikirim ke %s, want alamat baru %s", m1.To, newEmail)
	}
	first := tokenFrom(t, m1, "/konfirmasi-email")
	call(s, "POST", "/api/account/email", map[string]any{"newEmail": newEmail, "password": accountPassword}, here)
	second := tokenFrom(t, fm.next(t), "/konfirmasi-email")

	// belum dikonfirmasi: email lama tetap satu-satunya yang bisa login
	if c, _ := loginAs(s, oldEmail, accountPassword); c != http.StatusOK {
		t.Errorf("login email lama sebelum konfirmasi: %d, want 200", c)
	}
	if c, _ := loginAs(s, newEmail, accountPassword); c != http.StatusUnauthorized {
		t.Errorf("login email baru sebelum konfirmasi: %d, want 401", c)
	}
	if rec, _ := call(s, "POST", "/api/account/email/confirm", map[string]any{"token": first}); rec.Code != http.StatusBadRequest {
		t.Errorf("link yang sudah digantikan: %d, want 400", rec.Code)
	}
	if rec, _ := call(s, "POST", "/api/account/email/confirm", map[string]any{"token": "ngawur"}); rec.Code != http.StatusBadRequest {
		t.Errorf("token ngawur: %d, want 400", rec.Code)
	}

	rec, out := call(s, "POST", "/api/account/email/confirm", map[string]any{"token": second})
	if rec.Code != http.StatusOK || out["email"] != newEmail {
		t.Fatalf("konfirmasi: %d %v", rec.Code, out)
	}
	if m := fm.next(t); m.To != oldEmail || !strings.Contains(m.Text, "t***@mail.test") {
		t.Errorf("pemberitahuan ke alamat lama: to=%s\n%s", m.To, m.Text)
	}
	if c, _ := loginAs(s, newEmail, accountPassword); c != http.StatusOK {
		t.Errorf("login email baru: %d, want 200", c)
	}
	if c, _ := loginAs(s, oldEmail, accountPassword); c != http.StatusUnauthorized {
		t.Errorf("login email lama setelah konfirmasi: %d, want 401", c)
	}
	if !meVerified(t, s, here) {
		t.Error("email baru harus terverifikasi setelah konfirmasi")
	}
	// token tidak bisa dipakai ulang
	if rec, _ := call(s, "POST", "/api/account/email/confirm", map[string]any{"token": second}); rec.Code != http.StatusBadRequest {
		t.Errorf("token dipakai ulang: %d, want 400", rec.Code)
	}
}

func TestChangeEmailConfirmFailsIfTakenMeanwhile(t *testing.T) {
	s, fm, pool := emailServer(t)
	tag := randomHex(3)
	username, otherName := "tesakun_ew_"+tag, "tesakun_ew2_"+tag
	cleanupUser(t, pool, username)
	cleanupUser(t, pool, otherName)
	here := signupAs(t, s, fm, username)
	newEmail := username + "-baru@mail.test"
	call(s, "POST", "/api/account/email", map[string]any{"newEmail": newEmail, "password": accountPassword}, here)
	token := tokenFrom(t, fm.next(t), "/konfirmasi-email")

	// akun lain keburu memakai alamat itu
	rec, out := call(s, "POST", "/api/auth/signup", map[string]any{
		"username": otherName, "email": newEmail, "password": accountPassword, "name": "Penyerobot",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("signup penyerobot: %d %v", rec.Code, out)
	}
	fm.next(t)
	rec, _ = call(s, "POST", "/api/account/email/confirm", map[string]any{"token": token})
	if rec.Code != http.StatusConflict {
		t.Fatalf("konfirmasi saat email sudah dipakai: %d, want 409", rec.Code)
	}
	if c, _ := loginAs(s, username+"@mail.test", accountPassword); c != http.StatusOK {
		t.Errorf("email lama harus tetap berlaku: %d", c)
	}
}

func TestGoogleOnlyAccountCannotChangePasswordOrEmail(t *testing.T) {
	f := setupCheckout(t) // akun fixture tidak punya password (seperti akun Google)
	if rec, _ := call(f.s, "POST", "/api/account/password", map[string]any{"currentPassword": "x", "newPassword": "passwordbaru1"}, f.buyer); rec.Code != http.StatusBadRequest {
		t.Errorf("ganti password tanpa password: %d, want 400", rec.Code)
	}
	if rec, _ := call(f.s, "POST", "/api/account/email", map[string]any{"newEmail": "baru@mail.test", "password": "x"}, f.buyer); rec.Code != http.StatusBadRequest {
		t.Errorf("ganti email tanpa password: %d, want 400", rec.Code)
	}
}

// withPassword memberi akun fixture sebuah password.
func (f *checkoutFixture) withPassword(t *testing.T, userID string) {
	t.Helper()
	hash, _ := bcrypt.GenerateFromPassword([]byte(accountPassword), bcrypt.MinCost)
	if _, err := f.pool.Exec(context.Background(), `UPDATE users SET password_hash = $2 WHERE id = $1`, userID, string(hash)); err != nil {
		t.Fatal(err)
	}
}

func (f *checkoutFixture) deleteAccount(c *http.Cookie, body map[string]any) (int, map[string]any) {
	rec, out := call(f.s, "POST", "/api/account/delete", body, c)
	return rec.Code, out
}

func TestDeleteAccountBlockedWhileOrderNotFinal(t *testing.T) {
	f := setupCheckout(t)
	f.withPassword(t, f.buyerID)
	o := f.paidOrder(t, 1) // dibayar, belum selesai

	if code, _ := f.deleteAccount(f.buyer, map[string]any{"password": "salah"}); code != http.StatusForbidden {
		t.Errorf("password salah: %d, want 403", code)
	}
	if code, _ := f.deleteAccount(f.buyer, map[string]any{}); code != http.StatusForbidden {
		t.Errorf("tanpa password: %d, want 403", code)
	}
	code, out := f.deleteAccount(f.buyer, map[string]any{"password": accountPassword})
	if code != http.StatusConflict || !strings.Contains(out["error"].(string), "belum selesai") {
		t.Fatalf("pesanan belum final: %d %v, want 409", code, out)
	}
	if !loggedIn(f.s, f.buyer) {
		t.Fatal("penghapusan yang ditolak tidak boleh mengeluarkan sesi")
	}
	rec, chk := call(f.s, "GET", "/api/account/deletion", nil, f.buyer)
	if rec.Code != http.StatusOK || len(chk["blockers"].([]any)) != 1 {
		t.Errorf("cek penghalang: %d %v", rec.Code, chk)
	}

	// pesanan selesai → boleh
	f.complete(t, o)
	if code, out := f.deleteAccount(f.buyer, map[string]any{"password": accountPassword}); code != http.StatusOK {
		t.Fatalf("hapus setelah selesai: %d %v", code, out)
	}
}

func TestDeletedAccountCannotLoginAndOrdersStayForAdmin(t *testing.T) {
	f := setupCheckout(t)
	ctx := context.Background()
	f.withPassword(t, f.buyerID)
	username := f.buyerID // newSessionUser: username = id
	o := f.paidOrder(t, 2)
	f.complete(t, o)
	if _, err := f.pool.Exec(ctx, `INSERT INTO user_designs (user_id, title, product_type_id, color_id, uri) VALUES ($1,'Desain pribadi','kaos','putih','/uploads/x.png')`, f.buyerID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE users SET phone='0812', address='Jl. Rahasia', google_sub=$2 WHERE id=$1`, f.buyerID, "sub-"+f.tag); err != nil {
		t.Fatal(err)
	}
	if code, _ := loginAs(f.s, username, accountPassword); code != http.StatusOK {
		t.Fatalf("login sebelum hapus: %d", code)
	}
	_, secondSession := loginAs(f.s, username, accountPassword)

	if code, out := f.deleteAccount(f.buyer, map[string]any{"password": accountPassword}); code != http.StatusOK {
		t.Fatalf("hapus: %d %v", code, out)
	}
	if m := f.mailWith(t, "dihapus"); m.To != f.buyerID+"@mail.test" {
		t.Errorf("email penghapusan: %+v", m)
	}

	// tidak bisa login lagi dengan username/email lama, sesi lama mati
	for _, ident := range []string{username, f.buyerID + "@mail.test", "deleted-" + f.buyerID} {
		if c, _ := loginAs(f.s, ident, accountPassword); c != http.StatusUnauthorized {
			t.Errorf("login %q setelah dihapus: %d, want 401", ident, c)
		}
	}
	if loggedIn(f.s, f.buyer) || loggedIn(f.s, secondSession) {
		t.Error("semua sesi akun yang dihapus harus mati")
	}
	if rec, _ := call(f.s, "POST", "/api/auth/password/forgot", map[string]any{"email": f.buyerID + "@mail.test"}); rec.Code != http.StatusOK {
		t.Errorf("forgot: %d", rec.Code)
	}
	f.noMailWith(t, "Atur ulang") // tidak ada link reset untuk akun yang dihapus

	// data pribadi hilang, baris tetap ada
	var name, email, phone, address string
	var hasHash, hasSub, deleted bool
	if err := f.pool.QueryRow(ctx, `SELECT name, email, phone, address, password_hash IS NOT NULL, google_sub IS NOT NULL, deleted_at IS NOT NULL
		FROM users WHERE id = $1`, f.buyerID).Scan(&name, &email, &phone, &address, &hasHash, &hasSub, &deleted); err != nil {
		t.Fatal(err)
	}
	if name != "Pengguna dihapus" || strings.Contains(email, "mail.test") || phone != "" || address != "" || hasHash || hasSub || !deleted {
		t.Errorf("anonimisasi tidak lengkap: %q %q %q %q hash=%v sub=%v deleted=%v", name, email, phone, address, hasHash, hasSub, deleted)
	}
	var designs int
	f.pool.QueryRow(ctx, `SELECT count(*) FROM user_designs WHERE user_id = $1`, f.buyerID).Scan(&designs)
	if designs != 0 {
		t.Errorf("desain pribadi tersisa: %d", designs)
	}

	// pesanan tetap terlihat oleh admin, dengan nama dianonimkan dan item utuh
	rec, list := call(f.s, "GET", "/api/orders?q="+o, nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("daftar admin: %d %v", rec.Code, list)
	}
	orders := list["orders"].([]any)
	if len(orders) != 1 {
		t.Fatalf("pesanan di admin: %d, want 1", len(orders))
	}
	ord := orders[0].(map[string]any)
	cust := ord["customer"].(map[string]any)
	if cust["name"] != "Pengguna dihapus" || cust["address"] != "" || cust["phone"] != "" || cust["email"] != "" {
		t.Errorf("identitas pembeli di pesanan: %v", cust)
	}
	if ord["userDeleted"] != true || ord["username"] != nil && ord["username"] != "" {
		t.Errorf("penanda pengguna dihapus: userDeleted=%v username=%v", ord["userDeleted"], ord["username"])
	}
	if len(ord["items"].([]any)) != 1 || num(ord["total"]) == 0 || ord["status"] != "selesai" {
		t.Errorf("pesanan harus utuh: %v", ord)
	}
	if rec, one := call(f.s, "GET", "/api/orders/"+o, nil, f.admin); rec.Code != http.StatusOK || one["order"].(map[string]any)["userDeleted"] != true {
		t.Errorf("detail pesanan admin: %d %v", rec.Code, one)
	}

	// email & username bisa dipakai lagi untuk akun baru
	rec, out := call(f.s, "POST", "/api/auth/signup", map[string]any{
		"username": username, "email": f.buyerID + "@mail.test", "password": accountPassword, "name": "Orang Baru",
	})
	if rec.Code != http.StatusCreated {
		t.Errorf("daftar ulang dengan email lama: %d %v", rec.Code, out)
	}
	t.Cleanup(func() { f.pool.Exec(ctx, `DELETE FROM users WHERE username = $1`, username) })
}

func TestDeleteGoogleOnlyAccountNeedsEmailConfirmation(t *testing.T) {
	f := setupCheckout(t) // tanpa password
	if code, _ := f.deleteAccount(f.buyer, map[string]any{}); code != http.StatusBadRequest {
		t.Errorf("tanpa token: %d, want 400", code)
	}
	if code, _ := f.deleteAccount(f.buyer, map[string]any{"token": "ngawur"}); code != http.StatusBadRequest {
		t.Errorf("token ngawur: %d, want 400", code)
	}
	if rec, _ := call(f.s, "POST", "/api/account/delete/request", nil, f.buyer); rec.Code != http.StatusOK {
		t.Fatalf("minta link: %d", rec.Code)
	}
	m := f.mailWith(t, "Konfirmasi penghapusan")
	if m.To != f.buyerID+"@mail.test" {
		t.Fatalf("link dikirim ke %s", m.To)
	}
	token := tokenFrom(t, m, "/akun")

	// token milik akun lain tidak berlaku dan tidak hangus
	if code, _ := f.deleteAccount(f.other, map[string]any{"token": token}); code != http.StatusBadRequest {
		t.Errorf("token akun lain: %d, want 400", code)
	}
	if code, out := f.deleteAccount(f.buyer, map[string]any{"token": token}); code != http.StatusOK {
		t.Fatalf("hapus dengan token: %d %v", code, out)
	}
	f.mailWith(t, "dihapus") // email akun dihapus
	if code, _ := f.deleteAccount(f.buyer, map[string]any{"token": token}); code != http.StatusUnauthorized {
		t.Errorf("token dipakai ulang: %d, want 401 (sesi sudah mati)", code)
	}
}

func TestDeleteAccountBlockedForCreatorWithMoneyOwed(t *testing.T) {
	f := setupCheckout(t)
	ctx := context.Background()
	f.withPassword(t, "co_creator_"+f.tag)
	pw := map[string]any{"password": accountPassword}

	// royalti tertahan (pesanan belum selesai)
	o1 := f.paidOrder(t, 3)
	if code, out := f.deleteAccount(f.creator, pw); code != http.StatusConflict || !strings.Contains(out["error"].(string), "belum selesai") {
		t.Fatalf("royalti tertahan: %d %v, want 409", code, out)
	}

	// pesanan selesai → saldo tersedia belum ditarik
	f.complete(t, o1)
	if code, out := f.deleteAccount(f.creator, pw); code != http.StatusConflict || !strings.Contains(out["error"].(string), "belum ditarik") {
		t.Fatalf("saldo belum ditarik: %d %v, want 409", code, out)
	}

	// penarikan terbuka → tetap diblokir
	call(f.s, "PATCH", "/api/designer/bank", map[string]any{"bankName": "BCA", "account": "1234567890", "holder": "Kreator Tes"}, f.creator)
	bal := f.balance(t)
	code, out := f.withdraw(num(bal["available"]))
	if code != http.StatusCreated {
		t.Fatalf("tarik: %d %v", code, out)
	}
	if code, out := f.deleteAccount(f.creator, pw); code != http.StatusConflict || !strings.Contains(out["error"].(string), "penarikan") {
		t.Fatalf("penarikan terbuka: %d %v, want 409", code, out)
	}

	// admin mentransfer → bersih, boleh dihapus
	path := "/api/payouts/" + itoa(int64(num(out["payout"].(map[string]any)["id"])))
	if rec, o := call(f.s, "PATCH", path, map[string]any{"action": "process"}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("proses: %d %v", rec.Code, o)
	}
	if rec, o := call(f.s, "PATCH", path, map[string]any{"action": "paid", "reference": "TRX-HAPUS"}, f.admin); rec.Code != http.StatusOK {
		t.Fatalf("bayar: %d %v", rec.Code, o)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO design_submissions (id, designer_id, designer_name, title, product_type_id, color_id, price, uri)
		VALUES ($1, $2, 'Kreator', 'Pengajuan menunggu', 'kaos', 'putih', 150000, 'data:image/svg+xml,x')`, "ds-"+f.tag, "d-co-"+f.tag); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.pool.Exec(ctx, `DELETE FROM design_submissions WHERE id = $1`, "ds-"+f.tag) })

	if code, out := f.deleteAccount(f.creator, pw); code != http.StatusOK {
		t.Fatalf("hapus kreator: %d %v", code, out)
	}

	var active int
	f.pool.QueryRow(ctx, `SELECT count(*) FROM listings WHERE design_id = $1 AND active`, "dz-co-"+f.tag).Scan(&active)
	if active != 0 {
		t.Errorf("listing aktif tersisa: %d", active)
	}
	var dName, bank, status string
	f.pool.QueryRow(ctx, `SELECT name, COALESCE(bank_account,'') FROM designers WHERE id = $1`, "d-co-"+f.tag).Scan(&dName, &bank)
	f.pool.QueryRow(ctx, `SELECT status::text FROM design_submissions WHERE id = $1`, "ds-"+f.tag).Scan(&status)
	if dName != "Pengguna dihapus" || bank != "" || status != "ditolak" {
		t.Errorf("baris kreator: name=%q bank=%q submission=%q", dName, bank, status)
	}
	var payouts int
	f.pool.QueryRow(ctx, `SELECT count(*) FROM payouts WHERE designer_id = $1 AND status = 'dibayar'`, "d-co-"+f.tag).Scan(&payouts)
	if payouts != 1 {
		t.Errorf("riwayat penarikan harus tetap ada: %d", payouts)
	}
}

func TestAdminAccountCannotBeDeleted(t *testing.T) {
	f := setupCheckout(t)
	f.withPassword(t, "co_admin_"+f.tag)
	if code, _ := f.deleteAccount(f.admin, map[string]any{"password": accountPassword}); code != http.StatusConflict {
		t.Errorf("hapus admin: %d, want 409", code)
	}
}

// mailWith menunggu email yang subjeknya memuat substr (email lain, mis. dari
// alur pesanan, dilewati).
func (f *checkoutFixture) mailWith(t *testing.T, substr string) mail.Message {
	t.Helper()
	fm := f.s.auth.Mailer.(*fakeMailer)
	deadline := time.After(5 * time.Second)
	for {
		select {
		case m := <-fm.sent:
			if strings.Contains(m.Subject, substr) {
				return m
			}
		case <-deadline:
			t.Fatalf("email dengan subjek %q tidak terkirim", substr)
		}
	}
}

func (f *checkoutFixture) noMailWith(t *testing.T, substr string) {
	t.Helper()
	fm := f.s.auth.Mailer.(*fakeMailer)
	deadline := time.After(300 * time.Millisecond)
	for {
		select {
		case m := <-fm.sent:
			if strings.Contains(m.Subject, substr) {
				t.Fatalf("email tak terduga ke %s: %q", m.To, m.Subject)
			}
		case <-deadline:
			return
		}
	}
}
