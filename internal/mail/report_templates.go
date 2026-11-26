package mail

// ListingTakenDown tells a creator that a listing was deactivated after an
// IP / content report. The reporter's identity is never included.
func ListingTakenDown(to, name, listingTitle, reasonLabel, adminNote, dashboardLink string) Message {
	paragraphs := []string{
		"Listing \"" + listingTitle + "\" kami nonaktifkan dan tidak lagi tampil di katalog setelah ada laporan: " + reasonLabel + ".",
	}
	if adminNote != "" {
		paragraphs = append(paragraphs, "Catatan dari tim kami: "+adminNote)
	}
	paragraphs = append(paragraphs,
		"Pesanan yang sudah masuk tetap diproses. Kalau menurutmu ini keliru, hubungi tim KaryaKita dengan bukti kepemilikan desainmu agar kami tinjau ulang.")
	return render(to, "Listing kamu dinonaktifkan: "+listingTitle, content{
		Name:        name,
		Paragraphs:  paragraphs,
		ButtonURL:   dashboardLink,
		ButtonLabel: "Buka dashboard kreator",
		Footer:      "Email ini dikirim otomatis oleh sistem moderasi KaryaKita.",
	})
}
