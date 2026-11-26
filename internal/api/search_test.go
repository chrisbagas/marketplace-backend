package api

import (
	"context"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"karyakita/api/internal/db"
)

func TestSearchTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"  Kaos  BATIK ", []string{"kaos", "batik"}},
		{"kaos untuk ayah", []string{"kaos", "ayah"}}, // filler word dropped
		{"t-shirt", []string{"shirt"}},                // one-letter word dropped
		{"untuk", []string{"untuk"}},                  // ...unless nothing else is left
		{"kopi, kopi & KOPI!", []string{"kopi"}},      // duplicates
		{"' OR 1=1 --", []string{"or"}},               // only letters and digits survive
		{"a:* | b & !c", []string{"a", "b", "c"}},     // tsquery operators never reach Postgres
		{"", []string{}},
	}
	for _, c := range cases {
		if got := searchTokens(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("searchTokens(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	if got := searchTokens(strings.Repeat("kata ", 3) + "satu dua tiga empat lima enam tujuh delapan sembilan"); len(got) != searchMaxTokens {
		t.Errorf("token cap: got %d, want %d", len(got), searchMaxTokens)
	}
}

func TestEditDistance(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"kaos", "kaos", 0},
		{"koas", "kaos", 1},  // swapped letters
		{"batk", "batik", 1}, // missing letter
		{"hoodiee", "hoodie", 1},
		{"anmie", "anime", 1},
		{"kopi", "kaos", 3},
		{"", "abc", 3},
	}
	for _, c := range cases {
		if got := editDistance(c.a, c.b); got != c.want {
			t.Errorf("editDistance(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestBuildSQL(t *testing.T) {
	conds := []sqlPart{
		{sql: "l.active"},
		{group: "type", sql: "t = ANY(?)", args: []any{"kaos"}},
		{group: "price", sql: "p >= ?", args: []any{10}},
		{group: "price", sql: "p <= ?", args: []any{90}},
	}
	sql, args := buildSQL(part("SELECT x FROM y JOIN z ON a = ? WHERE true", "head"), conds, "type", part(" LIMIT ?", 5))
	wantSQL := "SELECT x FROM y JOIN z ON a = $1 WHERE true AND l.active AND p >= $2 AND p <= $3 LIMIT $4"
	if sql != wantSQL || !reflect.DeepEqual(args, []any{"head", 10, 90, 5}) {
		t.Errorf("buildSQL:\n got %s %v\nwant %s", sql, args, wantSQL)
	}
	if _, args := buildSQL(part("SELECT 1 WHERE true"), conds, "", part("")); len(args) != 3 {
		t.Errorf("no exclusion: %d args, want 3", len(args))
	}
}

func TestTsquery(t *testing.T) {
	p := parsedQuery{tokens: []string{"kaos", "batik"}, groups: [][]string{{"kaus", "baju"}, nil}}
	if got, want := p.tsquery(true), "(kaos:* | kaus | baju) & (batik:*)"; got != want {
		t.Errorf("tsquery(all) = %q, want %q", got, want)
	}
	if got, want := p.tsquery(false), "(kaos:* | kaus | baju) | (batik:*)"; got != want {
		t.Errorf("tsquery(any) = %q, want %q", got, want)
	}
}

// ---- integration (needs TEST_DATABASE_URL, see google_test.go) ----

// searchFixture: one creator with five listings whose words exist nowhere
// else in the catalog, so the assertions don't depend on seed data.
type searchFixture struct {
	*checkoutFixture
	word string // unique word present in every fixture listing
	ids  map[string]string
}

func setupSearch(t *testing.T) *searchFixture {
	f := &searchFixture{checkoutFixture: setupCheckout(t), ids: map[string]string{}}
	ctx := context.Background()
	tag := f.tag
	f.word = "zq" + tag // e.g. zq1a2b3c
	designer := "d-co-" + tag
	if _, err := f.pool.Exec(ctx, `UPDATE designers SET name = $2 WHERE id = $1`, designer, "Studio Wx"+tag); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO search_synonyms (terms) VALUES ($1)`, []string{"gunung" + tag, "bukit" + tag}); err != nil {
		t.Fatal(err)
	}
	designs := []struct {
		key, title string
		tags       []string
		attrs      []string
	}{
		{"naga", "Drakon" + tag + " Merah " + f.word, []string{"mitologi" + tag}, []string{"anime", "retro"}},
		{"gunung", "Pendaki Gunung" + tag + " " + f.word, []string{"petualang" + tag}, []string{"alam"}},
		{"kopi", "Seduh Pagi " + f.word, []string{"drakon" + tag}, []string{"kopi", "retro"}},
	}
	for _, d := range designs {
		id := "dz-s-" + d.key + "-" + tag
		if _, err := f.pool.Exec(ctx, `INSERT INTO designs (id, designer_id, title, uri, tags) VALUES ($1,$2,$3,'data:image/svg+xml,x',$4)`,
			id, designer, d.title, d.tags); err != nil {
			t.Fatal(err)
		}
		for _, a := range d.attrs {
			if _, err := f.pool.Exec(ctx, `INSERT INTO design_attributes (design_id, value_id) VALUES ($1,$2)`, id, a); err != nil {
				t.Fatal(err)
			}
		}
	}
	listings := []struct {
		key, design, ptype string
		price, sold        int
		colors             []string
		active             bool
	}{
		{"naga-kaos", "naga", "kaos", 150_000, 5, nil, true},
		{"naga-mug", "naga", "mug", 80_000, 50, nil, true},
		{"gunung-hoodie", "gunung", "hoodie", 300_000, 1, []string{"hitam"}, true},
		{"kopi-kaos", "kopi", "kaos", 120_000, 500, nil, true},
		{"kopi-off", "kopi", "kaos", 120_000, 900, nil, false},
	}
	all := []string{}
	for _, l := range listings {
		id := l.key + "-" + tag
		f.ids[l.key] = id
		all = append(all, id)
		if _, err := f.pool.Exec(ctx, `INSERT INTO listings (id, design_id, product_type_id, price, sold, color_ids, active) VALUES ($1,$2,$3,$4,$5,$6,$7)`,
			id, "dz-s-"+l.design+"-"+tag, l.ptype, l.price, l.sold, l.colors, l.active); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.ReindexSearch(ctx, f.pool, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.pool.Exec(ctx, `DELETE FROM listings WHERE id = ANY($1)`, all)
		f.pool.Exec(ctx, `DELETE FROM designs WHERE id LIKE $1`, "dz-s-%-"+tag)
		f.pool.Exec(ctx, `DELETE FROM search_synonyms WHERE $1 = ANY(terms)`, "gunung"+tag)
		f.pool.Exec(ctx, `DELETE FROM search_terms WHERE term LIKE $1`, "%"+tag+"%")
		db.ReindexSearch(ctx, f.pool, nil)
	})
	return f
}

// search runs GET /api/search and returns the listing ids in result order.
func (f *searchFixture) search(t *testing.T, params url.Values) ([]string, map[string]any) {
	t.Helper()
	rec, out := call(f.s, "GET", "/api/search?"+params.Encode(), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("search %v: %d %v", params, rec.Code, out)
	}
	ids := []string{}
	for _, p := range out["products"].([]any) {
		ids = append(ids, p.(map[string]any)["id"].(string))
	}
	return ids, out
}

func (f *searchFixture) want(t *testing.T, name string, got []string, keys ...string) {
	t.Helper()
	want := []string{}
	for _, k := range keys {
		want = append(want, f.ids[k])
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: got %v, want %v", name, got, want)
	}
}

func facetCount(t *testing.T, out map[string]any, group, id string) int {
	t.Helper()
	facets := out["facets"].(map[string]any)
	find := func(list []any) int {
		for _, v := range list {
			if m := v.(map[string]any); m["id"] == id {
				return num(m["count"])
			}
		}
		t.Fatalf("facet %s/%s tidak ada", group, id)
		return 0
	}
	if list, ok := facets[group].([]any); ok {
		return find(list)
	}
	for _, a := range facets["attributes"].([]any) {
		if m := a.(map[string]any); m["id"] == group {
			return find(m["values"].([]any))
		}
	}
	t.Fatalf("grup facet %s tidak ada", group)
	return 0
}

func TestSearchMatchingAndRanking(t *testing.T) {
	f := setupSearch(t)
	q := func(v string) url.Values { return url.Values{"q": {v}} }

	// the shared word finds every active listing; inactive ones never show
	ids, out := f.search(t, url.Values{"q": {f.word}, "sort": {"terlaris"}})
	f.want(t, "kata bersama", ids, "kopi-kaos", "naga-mug", "naga-kaos", "gunung-hoodie")
	if num(out["total"]) != 4 {
		t.Errorf("total %v, want 4", out["total"])
	}

	// title beats tag: the word is in the title of two listings and only a tag of the best seller
	ids, _ = f.search(t, q("drakon"+f.tag))
	f.want(t, "judul di atas tag", ids, "naga-mug", "naga-kaos", "kopi-kaos")

	// every word must match; product type is searchable
	ids, _ = f.search(t, q("mug drakon"+f.tag))
	f.want(t, "semua kata", ids, "naga-mug")

	// prefix: an unfinished word still matches
	ids, _ = f.search(t, q("mitol"))
	f.want(t, "awalan", ids, "naga-mug", "naga-kaos")

	// creator name: the creator owns the four search listings plus the one the
	// checkout fixture created for the same designer (active, "Desain Tes")
	ids, _ = f.search(t, q("wx"+f.tag))
	wantCreator := []string{f.ids["naga-kaos"], f.ids["naga-mug"], f.ids["gunung-hoodie"], f.ids["kopi-kaos"], f.listing}
	gotCreator := append([]string(nil), ids...)
	sort.Strings(gotCreator)
	sort.Strings(wantCreator)
	if !reflect.DeepEqual(gotCreator, wantCreator) {
		t.Errorf("nama kreator: got %v, want %v", gotCreator, wantCreator)
	}

	// attribute label is searchable ("Retro" is tagged on naga + kopi)
	ids, _ = f.search(t, url.Values{"q": {"retro " + f.word}, "sort": {"terlaris"}})
	f.want(t, "label atribut", ids, "kopi-kaos", "naga-mug", "naga-kaos")

	// synonym: bukit<tag> is declared equal to gunung<tag>
	ids, _ = f.search(t, q("bukit"+f.tag))
	f.want(t, "sinonim", ids, "gunung-hoodie")

	// typo: two letters swapped in a word the catalog knows
	typo := "mitoolgi" + f.tag
	ids, out = f.search(t, q(typo))
	f.want(t, "salah ketik", ids, "naga-mug", "naga-kaos")
	if used := out["query"].(map[string]any)["used"]; used != "mitologi"+f.tag {
		t.Errorf("koreksi: used = %v, want %q", used, "mitologi"+f.tag)
	}

	// no product has both words → partial matches, flagged
	ids, out = f.search(t, q("mitologi"+f.tag+" petualang"+f.tag))
	if len(ids) == 0 || out["query"].(map[string]any)["relaxed"] != true {
		t.Errorf("pencarian longgar: %v %v", ids, out["query"])
	}

	// nothing at all
	ids, out = f.search(t, q("xyzzy"+f.tag+"qqq"))
	if len(ids) != 0 || num(out["total"]) != 0 {
		t.Errorf("tanpa hasil: %v", ids)
	}

	// hostile input is just words
	for _, bad := range []string{"'; DROP TABLE listings; --", "a & | ! ( ) :*", "%_\\", strings.Repeat("x", 500)} {
		if rec, out := call(f.s, "GET", "/api/search?"+q(bad).Encode(), nil); rec.Code != http.StatusOK {
			t.Errorf("input %q: %d %v", bad, rec.Code, out)
		}
	}
}

func TestSearchFiltersFacetsAndPaging(t *testing.T) {
	f := setupSearch(t)
	base := func(extra ...string) url.Values {
		v := url.Values{"q": {f.word}, "sort": {"termurah"}}
		for i := 0; i+1 < len(extra); i += 2 {
			v.Set(extra[i], extra[i+1])
		}
		return v
	}

	ids, out := f.search(t, base())
	f.want(t, "termurah", ids, "naga-mug", "kopi-kaos", "naga-kaos", "gunung-hoodie")
	for _, c := range []struct {
		group, id string
		want      int
	}{
		{"types", "kaos", 2}, {"types", "mug", 1}, {"types", "hoodie", 1}, {"types", "totebag", 0},
		{"categories", "pakaian", 3}, {"categories", "rumah", 1},
		{"tema", "anime", 2}, {"tema", "alam", 1}, {"gaya", "retro", 3},
		{"colors", "hitam", 3}, // both kaos + the hoodie; mugs don't come in hitam
	} {
		if got := facetCount(t, out, c.group, c.id); got != c.want {
			t.Errorf("facet %s/%s = %d, want %d", c.group, c.id, got, c.want)
		}
	}
	price := out["facets"].(map[string]any)["price"].(map[string]any)
	if num(price["min"]) != 80_000 || num(price["max"]) != 300_000 {
		t.Errorf("rentang harga %v", price)
	}

	// a filter narrows the results, but its own group still shows the alternatives
	ids, out = f.search(t, base("type", "kaos"))
	f.want(t, "jenis", ids, "kopi-kaos", "naga-kaos")
	if facetCount(t, out, "types", "mug") != 1 || facetCount(t, out, "tema", "alam") != 0 {
		t.Errorf("facet setelah filter jenis: mug=%d alam=%d", facetCount(t, out, "types", "mug"), facetCount(t, out, "tema", "alam"))
	}

	ids, _ = f.search(t, base("type", "kaos,mug"))
	f.want(t, "dua jenis", ids, "naga-mug", "kopi-kaos", "naga-kaos")
	ids, _ = f.search(t, base("category", "pakaian"))
	f.want(t, "kategori", ids, "kopi-kaos", "naga-kaos", "gunung-hoodie")
	// same attribute = OR, different attributes = AND
	ids, _ = f.search(t, base("attr", "anime,alam"))
	f.want(t, "tema anime ATAU alam", ids, "naga-mug", "naga-kaos", "gunung-hoodie")
	ids, _ = f.search(t, base("attr", "anime,retro"))
	f.want(t, "tema anime DAN gaya retro", ids, "naga-mug", "naga-kaos")
	ids, _ = f.search(t, base("attr", "kopi,tidak-ada"))
	f.want(t, "nilai atribut tak dikenal diabaikan", ids, "kopi-kaos")
	ids, _ = f.search(t, base("min", "100000", "max", "200000"))
	f.want(t, "harga", ids, "kopi-kaos", "naga-kaos")
	ids, _ = f.search(t, base("designer", "d-co-"+f.tag, "sort", "termahal"))
	f.want(t, "kreator + termahal", ids, "gunung-hoodie", "naga-kaos", "kopi-kaos", "naga-mug")
	ids, _ = f.search(t, base("designer", "d-co2-"+f.tag))
	f.want(t, "kreator lain", ids)

	// colour: only the hoodie is restricted, and it has no putih
	ids, _ = f.search(t, base("color", "putih"))
	for _, id := range ids {
		if id == f.ids["gunung-hoodie"] {
			t.Errorf("hoodie hitam muncul di filter putih")
		}
	}

	// paging
	ids, out = f.search(t, base("limit", "3", "page", "2"))
	f.want(t, "halaman 2", ids, "gunung-hoodie")
	if num(out["pages"]) != 2 || num(out["total"]) != 4 || num(out["page"]) != 2 {
		t.Errorf("paging: %v/%v total %v", out["page"], out["pages"], out["total"])
	}
	_, out = f.search(t, base("limit", "3", "page", "99"))
	if num(out["page"]) != 2 {
		t.Errorf("halaman di luar batas: %v, want 2", out["page"])
	}
	if _, out = f.search(t, base("limit", "9999")); num(out["limit"]) != searchMaxLimit {
		t.Errorf("limit %v, want %d", out["limit"], searchMaxLimit)
	}
}

func TestSearchIndexFollowsListingChanges(t *testing.T) {
	f := setupSearch(t)
	q := url.Values{"q": {"mitologi" + f.tag}}

	// creator deactivates a listing → gone from search and from the vocabulary of corrections
	path := "/api/listings/" + f.ids["naga-kaos"]
	if rec, out := call(f.s, "PATCH", path, map[string]any{"active": false}, f.creator); rec.Code != http.StatusOK {
		t.Fatalf("nonaktifkan: %d %v", rec.Code, out)
	}
	ids, _ := f.search(t, q)
	f.want(t, "setelah nonaktif", ids, "naga-mug")

	// rename → findable by the new name right away
	if rec, out := call(f.s, "PATCH", "/api/listings/"+f.ids["naga-mug"], map[string]any{"title": "Cangkir Wyvern" + f.tag}, f.creator); rec.Code != http.StatusOK {
		t.Fatalf("ganti nama: %d %v", rec.Code, out)
	}
	ids, _ = f.search(t, url.Values{"q": {"wyvern" + f.tag}})
	f.want(t, "nama baru", ids, "naga-mug")
}

func TestSuggest(t *testing.T) {
	f := setupSearch(t)
	ctx := context.Background()
	popular := "drakon" + f.tag + " merah"
	if _, err := f.pool.Exec(ctx, `INSERT INTO search_terms (term, count, results) VALUES ($1, 9, 3), ($2, 99, 0)`,
		popular, "drakonzero"+f.tag); err != nil {
		t.Fatal(err)
	}

	rec, out := call(f.s, "GET", "/api/search/suggest?q=drako", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("suggest: %d %v", rec.Code, out)
	}
	terms := out["terms"].([]any)
	has := func(want string) bool {
		for _, v := range terms {
			if v == want {
				return true
			}
		}
		return false
	}
	if !has(popular) || !has("drakon"+f.tag) {
		t.Errorf("saran %v, want %q dan %q", terms, popular, "drakon"+f.tag)
	}
	if has("drakonzero" + f.tag) {
		t.Errorf("istilah tanpa hasil ikut disarankan: %v", terms)
	}

	// filter shortcut + creator + products
	_, out = call(f.s, "GET", "/api/search/suggest?q=anim", nil)
	filters := out["filters"].([]any)
	if len(filters) == 0 || filters[0].(map[string]any)["id"] != "anime" {
		t.Errorf("pintasan filter: %v", filters)
	}
	_, out = call(f.s, "GET", "/api/search/suggest?q="+url.QueryEscape("wx"+f.tag), nil)
	if creators := out["creators"].([]any); len(creators) != 1 || creators[0].(map[string]any)["id"] != "d-co-"+f.tag {
		t.Errorf("kreator: %v", out["creators"])
	}
	if products := out["products"].([]any); len(products) == 0 || len(products) > 4 {
		t.Errorf("produk di saran: %d", len(products))
	}

	// empty query → popular searches, never ones that found nothing
	_, out = call(f.s, "GET", "/api/search/suggest", nil)
	for _, v := range out["terms"].([]any) {
		if v == "drakonzero"+f.tag {
			t.Errorf("populer memuat istilah tanpa hasil")
		}
	}
}

func TestSearchLogsResultCount(t *testing.T) {
	f := setupSearch(t)
	ctx := context.Background()
	results := func(term string) (n *int) {
		f.pool.QueryRow(ctx, `SELECT results FROM search_terms WHERE term = $1`, term).Scan(&n)
		return n
	}
	f.search(t, url.Values{"q": {"Mitologi" + f.tag}})
	if n := results("mitologi" + f.tag); n == nil || *n != 2 {
		t.Errorf("hasil tercatat %v, want 2", n)
	}
	miss := "hilang" + f.tag + "zz"
	f.search(t, url.Values{"q": {miss}})
	if n := results(miss); n == nil || *n != 0 {
		t.Errorf("tanpa hasil tercatat %v, want 0", n)
	}
	// a filtered search says nothing about the catalog as a whole
	f.search(t, url.Values{"q": {"petualang" + f.tag}, "type": {"mug"}})
	if n := results("petualang" + f.tag); n != nil {
		t.Errorf("pencarian berfilter tercatat sebagai tanpa hasil")
	}

	rec, out := call(f.s, "GET", "/api/search/insights", nil, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("insights: %d %v", rec.Code, out)
	}
	found := false
	for _, v := range out["noResults"].([]any) {
		found = found || v.(map[string]any)["term"] == miss
	}
	if !found {
		t.Errorf("istilah tanpa hasil tidak muncul di laporan admin")
	}
	if rec, _ := call(f.s, "GET", "/api/search/insights", nil, f.creator); rec.Code != http.StatusForbidden {
		t.Errorf("kreator membuka laporan pencarian: %d, want 403", rec.Code)
	}
}

func TestSynonymAndAttributeAdmin(t *testing.T) {
	f := setupSearch(t)
	ctx := context.Background()
	a, b := "alpha"+f.tag, "petualang"+f.tag

	// before: the made-up word finds nothing
	if ids, _ := f.search(t, url.Values{"q": {a}}); len(ids) != 0 {
		t.Fatalf("sebelum sinonim: %v", ids)
	}
	if rec, _ := call(f.s, "POST", "/api/search/synonyms", map[string]any{"terms": []string{a, b}}, f.creator); rec.Code != http.StatusForbidden {
		t.Errorf("kreator menambah sinonim: %d, want 403", rec.Code)
	}
	if rec, _ := call(f.s, "POST", "/api/search/synonyms", map[string]any{"terms": []string{a}}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("satu kata saja: %d, want 400", rec.Code)
	}
	if rec, _ := call(f.s, "POST", "/api/search/synonyms", map[string]any{"terms": []string{a, "dua kata"}}, f.admin); rec.Code != http.StatusBadRequest {
		t.Errorf("frasa: %d, want 400", rec.Code)
	}
	rec, out := call(f.s, "POST", "/api/search/synonyms", map[string]any{"terms": []string{" " + strings.ToUpper(a), b}}, f.admin)
	if rec.Code != http.StatusCreated {
		t.Fatalf("tambah sinonim: %d %v", rec.Code, out)
	}
	id := int64(num(out["synonym"].(map[string]any)["id"]))
	t.Cleanup(func() { f.pool.Exec(ctx, `DELETE FROM search_synonyms WHERE id = $1`, id) })
	ids, _ := f.search(t, url.Values{"q": {a}})
	f.want(t, "sinonim baru", ids, "gunung-hoodie")
	if rec, _ := call(f.s, "DELETE", "/api/search/synonyms/"+itoa(id), nil, f.admin); rec.Code != http.StatusOK {
		t.Errorf("hapus sinonim: %d", rec.Code)
	}
	if ids, _ := f.search(t, url.Values{"q": {a}}); len(ids) != 0 {
		t.Errorf("setelah hapus sinonim: %v", ids)
	}

	// attribute value: add → tag a design → searchable by label → delete untags
	label := "Wayang " + f.tag
	valueID := slugify(label)
	t.Cleanup(func() { f.pool.Exec(ctx, `DELETE FROM attribute_values WHERE id = $1`, valueID) })
	if rec, out := call(f.s, "POST", "/api/attributes/tema/values", map[string]any{"label": label, "emoji": "🎭"}, f.admin); rec.Code != http.StatusCreated {
		t.Fatalf("tambah nilai atribut: %d %v", rec.Code, out)
	}
	if rec, _ := call(f.s, "POST", "/api/attributes/tema/values", map[string]any{"label": label}, f.admin); rec.Code != http.StatusConflict {
		t.Errorf("nama ganda: %d, want 409", rec.Code)
	}
	if rec, _ := call(f.s, "POST", "/api/attributes/tidak-ada/values", map[string]any{"label": "X"}, f.admin); rec.Code != http.StatusNotFound {
		t.Errorf("atribut tak dikenal: %d, want 404", rec.Code)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO design_attributes (design_id, value_id) VALUES ($1, $2)`, "dz-s-gunung-"+f.tag, valueID); err != nil {
		t.Fatal(err)
	}
	if rec, _ := call(f.s, "PATCH", "/api/attribute-values/"+valueID, map[string]any{"label": "Wayang Kulit " + f.tag}, f.admin); rec.Code != http.StatusOK {
		t.Errorf("ganti nama: %d", rec.Code)
	}
	ids, _ = f.search(t, url.Values{"q": {"kulit " + f.tag}})
	f.want(t, "label atribut baru", ids, "gunung-hoodie")
	ids, _ = f.search(t, url.Values{"attr": {valueID}})
	f.want(t, "filter atribut baru", ids, "gunung-hoodie")
	if rec, _ := call(f.s, "DELETE", "/api/attribute-values/"+valueID, nil, f.admin); rec.Code != http.StatusOK {
		t.Errorf("hapus nilai: %d", rec.Code)
	}
	if ids, _ := f.search(t, url.Values{"q": {"kulit " + f.tag}}); len(ids) != 0 {
		t.Errorf("setelah hapus nilai: %v", ids)
	}
}

func TestSubmissionAttributes(t *testing.T) {
	f := setupSearch(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE users SET email_verified = true WHERE id = $1`, "co_creator_"+f.tag); err != nil {
		t.Fatal(err)
	}
	tags := []string{}
	for i := range 20 {
		tags = append(tags, "Tag"+itoa(int64(i)))
	}
	rec, out := call(f.s, "POST", "/api/designs", map[string]any{
		"title": "Garuda Emas " + f.word, "type": "kaos", "color": "putih", "price": 140_000, "uri": "data:image/svg+xml,x",
		"tags": tags,
		// tema max 3, gaya max 2; unknown ids and duplicates are dropped
		"attributes": []string{"anime", "alam", "kopi", "laut", "retro", "retro", "bukan-atribut", "lucu", "minimalis"},
	}, f.creator)
	if rec.Code != http.StatusOK {
		t.Fatalf("ajukan: %d %v", rec.Code, out)
	}
	d := out["design"].(map[string]any)
	subID := d["id"].(string)
	t.Cleanup(func() { f.pool.Exec(ctx, `DELETE FROM design_submissions WHERE id = $1`, subID) })
	if got := strs(d["attributes"]); !reflect.DeepEqual(got, []string{"anime", "alam", "kopi", "retro", "lucu"}) {
		t.Errorf("atribut tersimpan %v", got)
	}
	if got := strs(d["tags"]); len(got) != maxTags || got[0] != "tag0" {
		t.Errorf("tag tersimpan %d (%v), want %d", len(got), got, maxTags)
	}

	// approval publishes the listing and makes it searchable at once
	rec, out = call(f.s, "PATCH", "/api/designs", map[string]any{"id": subID, "status": "disetujui"}, f.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("setujui: %d %v", rec.Code, out)
	}
	listing := out["design"].(map[string]any)["listingId"].(string)
	t.Cleanup(func() {
		f.pool.Exec(ctx, `UPDATE design_submissions SET published_listing_id = NULL WHERE id = $1`, subID)
		var design string
		f.pool.QueryRow(ctx, `DELETE FROM listings WHERE id = $1 RETURNING design_id`, listing).Scan(&design)
		f.pool.Exec(ctx, `DELETE FROM designs WHERE id = $1`, design)
	})
	ids, _ := f.search(t, url.Values{"q": {"garuda " + f.word}})
	if !reflect.DeepEqual(ids, []string{listing}) {
		t.Errorf("listing baru: %v, want [%s]", ids, listing)
	}
	rec, out = call(f.s, "GET", "/api/products/"+listing, nil)
	p := out["product"].(map[string]any)
	if rec.Code != http.StatusOK || p["category"] != "pakaian" || len(strs(p["attributes"])) != 5 {
		t.Errorf("produk: %d kategori=%v atribut=%v", rec.Code, p["category"], p["attributes"])
	}
}

func TestTaxonomy(t *testing.T) {
	f := setupSearch(t)
	rec, out := call(f.s, "GET", "/api/taxonomy", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("taxonomy: %d %v", rec.Code, out)
	}
	wrapped := map[string]any{"facets": out}
	if n := facetCount(t, wrapped, "types", "kaos"); n < 2 {
		t.Errorf("jumlah kaos %d, want ≥ 2", n)
	}
	for _, c := range [][2]string{{"categories", "pakaian"}, {"tema", "anime"}, {"gaya", "retro"}, {"acara", "lebaran"}, {"colors", "hitam"}} {
		facetCount(t, wrapped, c[0], c[1]) // fails the test when missing
	}
	for _, a := range out["attributes"].([]any) {
		if m := a.(map[string]any); m["id"] == "tema" && num(m["max"]) != 3 {
			t.Errorf("tema max = %v, want 3", m["max"])
		}
	}
}

func strs(v any) []string {
	out := []string{}
	list, _ := v.([]any)
	for _, s := range list {
		out = append(out, s.(string))
	}
	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
