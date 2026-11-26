package mail

// Email pengaturan akun: ganti email dan hapus akun (Bahasa Indonesia).

// ChangeEmailVerify dikirim ke alamat BARU; email di akun baru berganti setelah link dibuka.
func ChangeEmailVerify(to, name, link string) Message {
	return render(to, "Konfirmasi email baru akun KaryaKita", content{
		Name: name,
		Paragraphs: []string{
			"Kamu meminta email ini dipakai sebagai email baru akun KaryaKita. Email akun baru berganti setelah kamu mengonfirmasinya.",
			"Link berlaku 24 jam dan hanya bisa dipakai sekali.",
		},
		ButtonURL: link, ButtonLabel: "Konfirmasi email baru",
		Footer: "Tidak merasa meminta? Abaikan email ini — email akunmu tidak berubah.",
	})
}

// EmailChangedNotice dikirim ke alamat LAMA setelah penggantian dikonfirmasi.
func EmailChangedNotice(to, name, maskedNew, forgotLink string) Message {
	return render(to, "Email akun KaryaKita kamu diganti", content{
		Name: name,
		Paragraphs: []string{
			"Email akunmu baru saja diganti menjadi " + maskedNew + ". Mulai sekarang login dan email dari KaryaKita memakai alamat baru itu.",
			"Kalau ini bukan kamu, segera amankan akun lewat tombol di bawah dan hubungi kami.",
		},
		ButtonURL: forgotLink, ButtonLabel: "Amankan akun",
		Footer: "Email ini dikirim otomatis demi keamanan akunmu.",
	})
}

// DeleteAccountConfirm: konfirmasi penghapusan untuk akun tanpa password (Google).
func DeleteAccountConfirm(to, name, link string) Message {
	return render(to, "Konfirmasi penghapusan akun KaryaKita", content{
		Name: name,
		Paragraphs: []string{
			"Kami menerima permintaan untuk menghapus akun KaryaKita-mu. Data pribadimu akan dianonimkan dan tidak bisa dipulihkan.",
			"Link berlaku 30 menit dan hanya bisa dipakai sekali. Kamu masih perlu menekan tombol konfirmasi terakhir di halaman Akun.",
		},
		ButtonURL: link, ButtonLabel: "Lanjutkan penghapusan akun",
		Footer: "Tidak merasa meminta? Abaikan email ini dan segera ganti akses ke akunmu — akunmu belum dihapus.",
	})
}

// AccountDeleted: bukti bahwa penghapusan selesai, dikirim ke email terakhir akun.
func AccountDeleted(to, name string) Message {
	return render(to, "Akun KaryaKita kamu sudah dihapus", content{
		Name: name,
		Paragraphs: []string{
			"Akunmu sudah dihapus: nama, email, nomor telepon, alamat, dan kredensial login telah dianonimkan.",
			"Riwayat pesanan tetap disimpan tanpa identitasmu untuk keperluan pembukuan.",
		},
		Footer: "Kamu selalu bisa mendaftar lagi dengan email yang sama sebagai akun baru.",
	})
}
