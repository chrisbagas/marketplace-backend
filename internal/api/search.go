package api

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Catalog search (GET /api/search) and autocomplete (GET /api/search/suggest).
//
// A query goes through four steps:
//  1. tokenize: lowercase words, filler words dropped;
//  2. correct: a word the catalog doesn't know is replaced by the closest
//     word it does know (typo tolerance, "koas" -> "kaos");
//  3. expand: each word also matches its synonyms (search_synonyms);
//  4. match: every word must match (prefix match, so "bat" finds "batik").
//     If nothing matches all words, fall back to matching any word.
//
// Results are ranked by text relevance (title > tags/attributes > product
// type > creator) boosted by sales and rating, then narrowed by filters.
// Each filter group reports counts computed without its own filter, so the
// numbers show what picking another option would give.

const (
	searchDefaultLimit = 24
	searchMaxLimit     = 60
	searchMaxTokens    = 8
)

// words that carry no meaning in a product search
var searchStopWords = map[string]bool{
	"dan": true, "yang": true, "untuk": true, "buat": true, "di": true, "ke": true,
	"dari": true, "dengan": true, "atau": true, "the": true, "for": true, "and": true, "of": true,
}

// searchTokens splits a query the same way Postgres' 'simple' parser splits
// documents: lowercase runs of letters and digits.
func searchTokens(q string) []string {
	fields := strings.FieldsFunc(strings.ToLower(q), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := map[string]bool{}
	all := []string{}
	for _, f := range fields {
		if utf8.RuneCountInString(f) > 30 {
			f = string([]rune(f)[:30])
		}
		if seen[f] {
			continue
		}
		seen[f] = true
		all = append(all, f)
	}
	// drop filler and one-letter words unless nothing else is left
	out := []string{}
	for _, f := range all {
		if !searchStopWords[f] && utf8.RuneCountInString(f) > 1 {
			out = append(out, f)
		}
	}
	if len(out) == 0 {
		out = all
	}
	if len(out) > searchMaxTokens {
		out = out[:searchMaxTokens]
	}
	return out
}

// editDistance is the optimal string alignment distance: insertions,
// deletions, substitutions and swaps of two adjacent letters each cost 1.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev2 := make([]int, len(rb)+1)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(rb)]
}

// maxTypos: how many edits a word of this length may be away from a known
// word. Short words are left alone: almost any 3-letter word is one edit
// from another.
func maxTypos(runes int) int {
	switch {
	case runes < 4:
		return 0
	case runes < 7:
		return 1
	default:
		return 2
	}
}

// parsedQuery is a search query after correction and synonym expansion.
type parsedQuery struct {
	tokens    []string   // words actually searched (after correction)
	groups    [][]string // per token: its synonyms
	corrected bool
}

// tsquery renders the tokens as a to_tsquery('simple') expression. Tokens are
// letters and digits only, so they need no escaping.
func (p parsedQuery) tsquery(matchAll bool) string {
	parts := make([]string, len(p.tokens))
	for i, t := range p.tokens {
		alts := append([]string{t + ":*"}, p.groups[i]...)
		parts[i] = "(" + strings.Join(alts, " | ") + ")"
	}
	if matchAll {
		return strings.Join(parts, " & ")
	}
	return strings.Join(parts, " | ")
}

func (p parsedQuery) text() string { return strings.Join(p.tokens, " ") }

func (s *Server) parseQuery(ctx context.Context, q string) (parsedQuery, error) {
	p := parsedQuery{tokens: searchTokens(q)}
	if len(p.tokens) == 0 {
		return p, nil
	}
	// the synonym table is small and curated by admin: read it whole, so a
	// corrected word still finds its synonyms
	synonyms := map[string][]string{}
	rows, err := s.pool.Query(ctx, `SELECT terms FROM search_synonyms`)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var terms []string
		if err := rows.Scan(&terms); err != nil {
			rows.Close()
			return p, err
		}
		for _, t := range terms {
			for _, other := range terms {
				if other != t {
					synonyms[t] = append(synonyms[t], other)
				}
			}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return p, err
	}

	for i, tok := range p.tokens {
		if _, isSynonym := synonyms[tok]; !isSynonym {
			fixed, err := s.correctWord(ctx, tok)
			if err != nil {
				return p, err
			}
			if fixed != tok {
				p.tokens[i], p.corrected = fixed, true
			}
		}
		p.groups = append(p.groups, synonyms[p.tokens[i]])
	}
	return p, nil
}

// correctWord returns the closest catalog word when tok is not the start of
// any known word, or tok itself when it is known or nothing is close enough.
func (s *Server) correctWord(ctx context.Context, tok string) (string, error) {
	n := utf8.RuneCountInString(tok)
	limit := maxTypos(n)
	if limit == 0 {
		return tok, nil
	}
	var known bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM search_words WHERE word LIKE $1 || '%')`, tok).Scan(&known); err != nil {
		return tok, err
	}
	if known {
		return tok, nil
	}
	// candidates: similar length and either the same first letter or enough
	// shared trigrams (covers a typo in the first letter of a longer word)
	rows, err := s.pool.Query(ctx, `
		SELECT word, ndoc FROM search_words
		WHERE char_length(word) BETWEEN $2 AND $3 AND (left(word, 1) = left($1, 1) OR word % $1)
		LIMIT 5000`, tok, n-limit, n+limit)
	if err != nil {
		return tok, err
	}
	defer rows.Close()
	best, bestDist, bestDocs := tok, limit+1, 0
	for rows.Next() {
		var word string
		var ndoc int
		if err := rows.Scan(&word, &ndoc); err != nil {
			return tok, err
		}
		if d := editDistance(tok, word); d < bestDist || (d == bestDist && ndoc > bestDocs) {
			best, bestDist, bestDocs = word, d, ndoc
		}
	}
	return best, rows.Err()
}

// ---- filters ---------------------------------------------------------------

type searchFilters struct {
	Q        string
	Types    []string
	Category string
	Attrs    []string // attribute value ids; OR within an attribute, AND across attributes
	Color    string
	Min, Max int
	Designer string
	Sort     string
	Page     int
	Limit    int
}

func (f searchFilters) filtered() bool {
	return len(f.Types) > 0 || f.Category != "" || len(f.Attrs) > 0 || f.Color != "" || f.Min > 0 || f.Max > 0 || f.Designer != ""
}

func splitList(v string) []string {
	out := []string{}
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" && len(out) < 30 {
			out = append(out, p)
		}
	}
	return out
}

func readSearchFilters(r *http.Request) searchFilters {
	q := r.URL.Query()
	num := func(key string) int {
		n, _ := strconv.Atoi(q.Get(key))
		return max(n, 0)
	}
	f := searchFilters{
		Q:        strings.TrimSpace(q.Get("q")),
		Types:    splitList(q.Get("type")),
		Category: q.Get("category"),
		Attrs:    splitList(q.Get("attr")),
		Color:    q.Get("color"),
		Min:      num("min"),
		Max:      num("max"),
		Designer: q.Get("designer"),
		Sort:     q.Get("sort"),
		Page:     max(num("page"), 1),
		Limit:    num("limit"),
	}
	if utf8.RuneCountInString(f.Q) > 100 {
		f.Q = string([]rune(f.Q)[:100])
	}
	if f.Limit == 0 {
		f.Limit = searchDefaultLimit
	}
	f.Limit = min(f.Limit, searchMaxLimit)
	return f
}

// sqlPart is a piece of SQL with `?` placeholders and their values.
// group names the filter it belongs to, so a facet can leave it out.
type sqlPart struct {
	group string
	sql   string
	args  []any
}

func part(sql string, args ...any) sqlPart { return sqlPart{sql: sql, args: args} }

// buildSQL joins head + " AND cond" for every condition outside `exclude` +
// tail, and numbers the placeholders. Leaving out a condition also leaves out
// its arguments: Postgres rejects parameters that the query never mentions.
func buildSQL(head sqlPart, conds []sqlPart, exclude string, tail sqlPart) (string, []any) {
	var sb strings.Builder
	args := []any{}
	write := func(p sqlPart) {
		for _, seg := range strings.SplitAfter(p.sql, "?") {
			if strings.HasSuffix(seg, "?") {
				sb.WriteString(seg[:len(seg)-1])
				fmt.Fprintf(&sb, "$%d", len(args)+1)
				args = append(args, p.args[0])
				p.args = p.args[1:]
			} else {
				sb.WriteString(seg)
			}
		}
	}
	write(head)
	for _, c := range conds {
		if exclude != "" && c.group == exclude {
			continue
		}
		sb.WriteString(" AND ")
		write(c)
	}
	write(tail)
	return sb.String(), args
}

const matchSQL = `EXISTS (SELECT 1 FROM listing_search ls WHERE ls.listing_id = l.id AND ls.doc @@ to_tsquery('simple', ?))`

// relevance: text rank (weights D, C, B, A = creator, product type, tags and
// attributes, title) lifted by sales and rating. The lift tops out around
// 2.3x at 50.000 sold with a 5.0 rating, below the 2.5x gap between a title
// match and a tag match: popularity orders equally good matches, it does not
// push a weak match over a strong one.
const relevanceSQL = `(SELECT ts_rank_cd('{0.1, 0.2, 0.4, 1.0}', ls.doc, to_tsquery('simple', ?)) FROM listing_search ls WHERE ls.listing_id = l.id)
	* (1 + ln(1 + l.sold) / 10.0) * (1 + l.rating / 50.0)`

func (f searchFilters) conds(tsquery string, attrOf map[string]string) []sqlPart {
	conds := []sqlPart{{sql: "l.active"}}
	add := func(group, sql string, args ...any) {
		conds = append(conds, sqlPart{group: group, sql: sql, args: args})
	}
	if tsquery != "" {
		add("q", matchSQL, tsquery)
	}
	if len(f.Types) > 0 {
		add("type", `l.product_type_id = ANY(?)`, f.Types)
	}
	if f.Category != "" {
		add("category", `pt.category_id = ?`, f.Category)
	}
	byAttr := map[string][]string{}
	order := []string{}
	for _, v := range f.Attrs {
		a, ok := attrOf[v]
		if !ok {
			continue // unknown value: ignore instead of returning nothing
		}
		if _, seen := byAttr[a]; !seen {
			order = append(order, a)
		}
		byAttr[a] = append(byAttr[a], v)
	}
	for _, a := range order {
		add("attr:"+a, `EXISTS (SELECT 1 FROM design_attributes da WHERE da.design_id = d.id AND da.value_id = ANY(?))`, byAttr[a])
	}
	if f.Color != "" {
		add("color", `? = ANY(`+effectiveColors+`)`, f.Color)
	}
	if f.Min > 0 {
		add("price", `l.price >= ?`, f.Min)
	}
	if f.Max > 0 {
		add("price", `l.price <= ?`, f.Max)
	}
	if f.Designer != "" {
		add("designer", `dr.id = ?`, f.Designer)
	}
	return conds
}

func (f searchFilters) orderBy(tsquery string) sqlPart {
	sort := f.Sort
	if sort == "" || (sort == "relevan" && tsquery == "") {
		sort = "terlaris"
		if tsquery != "" {
			sort = "relevan"
		}
	}
	switch sort {
	case "relevan":
		return part(` ORDER BY `+relevanceSQL+` DESC, l.sold DESC, l.id`, tsquery)
	case "terbaru":
		return part(` ORDER BY l.created_at DESC, l.id`)
	case "termurah":
		return part(` ORDER BY l.price, l.sold DESC, l.id`)
	case "termahal":
		return part(` ORDER BY l.price DESC, l.sold DESC, l.id`)
	case "rating":
		return part(` ORDER BY l.rating DESC, l.sold DESC, l.id`)
	default:
		return part(` ORDER BY l.sold DESC, l.id`)
	}
}

// ---- taxonomy + facets -----------------------------------------------------

type FacetValue struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Emoji string `json:"emoji,omitempty"`
	Hex   string `json:"hex,omitempty"`      // colours
	Group string `json:"category,omitempty"` // product types: their category
	Count int    `json:"count"`
}

type FacetAttribute struct {
	ID     string       `json:"id"`
	Label  string       `json:"label"`
	Max    int          `json:"max"` // how many values one design may carry
	Values []FacetValue `json:"values"`
}

// Facets is the whole taxonomy with, per option, how many products it holds.
type Facets struct {
	Categories []FacetValue     `json:"categories"`
	Types      []FacetValue     `json:"types"`
	Attributes []FacetAttribute `json:"attributes"`
	Colors     []FacetValue     `json:"colors"`
	Price      struct {
		Min int `json:"min"`
		Max int `json:"max"`
	} `json:"price"`
}

// attrOf maps an attribute value id to its attribute id.
func (f *Facets) attrOf() map[string]string {
	m := map[string]string{}
	for _, a := range f.Attributes {
		for _, v := range a.Values {
			m[v.ID] = a.ID
		}
	}
	return m
}

// loadTaxonomy reads categories, product types, attributes and colours (counts zero).
func (s *Server) loadTaxonomy(ctx context.Context) (*Facets, error) {
	f := &Facets{Categories: []FacetValue{}, Types: []FacetValue{}, Attributes: []FacetAttribute{}, Colors: []FacetValue{}}
	list := func(sql string, scan func(row interface{ Scan(...any) error }) error) error {
		rows, err := s.pool.Query(ctx, sql)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := scan(rows); err != nil {
				return err
			}
		}
		return rows.Err()
	}
	if err := list(`SELECT id, label, emoji FROM categories ORDER BY sort, label`, func(row interface{ Scan(...any) error }) error {
		var v FacetValue
		err := row.Scan(&v.ID, &v.Label, &v.Emoji)
		f.Categories = append(f.Categories, v)
		return err
	}); err != nil {
		return nil, err
	}
	if err := list(`SELECT id, label, COALESCE(category_id, '') FROM product_types ORDER BY sort, label`, func(row interface{ Scan(...any) error }) error {
		var v FacetValue
		err := row.Scan(&v.ID, &v.Label, &v.Group)
		f.Types = append(f.Types, v)
		return err
	}); err != nil {
		return nil, err
	}
	if err := list(`SELECT id, label, max_per_design FROM attributes ORDER BY sort, label`, func(row interface{ Scan(...any) error }) error {
		a := FacetAttribute{Values: []FacetValue{}}
		err := row.Scan(&a.ID, &a.Label, &a.Max)
		f.Attributes = append(f.Attributes, a)
		return err
	}); err != nil {
		return nil, err
	}
	if err := list(`SELECT attribute_id, id, label, emoji FROM attribute_values ORDER BY sort, label`, func(row interface{ Scan(...any) error }) error {
		var attr string
		var v FacetValue
		if err := row.Scan(&attr, &v.ID, &v.Label, &v.Emoji); err != nil {
			return err
		}
		for i := range f.Attributes {
			if f.Attributes[i].ID == attr {
				f.Attributes[i].Values = append(f.Attributes[i].Values, v)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if err := list(`SELECT id, label, hex FROM colors ORDER BY label`, func(row interface{ Scan(...any) error }) error {
		var v FacetValue
		err := row.Scan(&v.ID, &v.Label, &v.Hex)
		f.Colors = append(f.Colors, v)
		return err
	}); err != nil {
		return nil, err
	}
	return f, nil
}

// countFacets fills the counts of tax for the given conditions. Each group is
// counted with every filter applied except its own.
func (s *Server) countFacets(ctx context.Context, tax *Facets, conds []sqlPart) error {
	counts := func(head sqlPart, exclude, tail string) (map[string]int, error) {
		sql, args := buildSQL(head, conds, exclude, part(tail))
		rows, err := s.pool.Query(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		m := map[string]int{}
		for rows.Next() {
			var id *string
			var n int
			if err := rows.Scan(&id, &n); err != nil {
				return nil, err
			}
			if id != nil {
				m[*id] = n
			}
		}
		return m, rows.Err()
	}
	fill := func(values []FacetValue, m map[string]int) {
		for i := range values {
			values[i].Count = m[values[i].ID]
		}
	}
	const where = ` WHERE true`

	m, err := counts(part(`SELECT pt.category_id, count(*)`+productJoins+where), "category", ` GROUP BY 1`)
	if err != nil {
		return err
	}
	fill(tax.Categories, m)

	if m, err = counts(part(`SELECT l.product_type_id, count(*)`+productJoins+where), "type", ` GROUP BY 1`); err != nil {
		return err
	}
	fill(tax.Types, m)

	for i, a := range tax.Attributes {
		head := part(`SELECT da.value_id, count(DISTINCT l.id)`+productJoins+`
			JOIN design_attributes da ON da.design_id = d.id
			JOIN attribute_values av ON av.id = da.value_id AND av.attribute_id = ?`+where, a.ID)
		if m, err = counts(head, "attr:"+a.ID, ` GROUP BY 1`); err != nil {
			return err
		}
		fill(tax.Attributes[i].Values, m)
	}

	if m, err = counts(part(`SELECT c, count(*) FROM (SELECT `+effectiveColors+` AS cols`+productJoins+where), "color",
		`) x, unnest(x.cols) AS c GROUP BY 1`); err != nil {
		return err
	}
	fill(tax.Colors, m)

	sql, args := buildSQL(part(`SELECT COALESCE(min(l.price), 0), COALESCE(max(l.price), 0)`+productJoins+where), conds, "price", part(""))
	return s.pool.QueryRow(ctx, sql, args...).Scan(&tax.Price.Min, &tax.Price.Max)
}

// ---- search ----------------------------------------------------------------

type searchResult struct {
	Products []Product `json:"products"`
	Total    int       `json:"total"`
	Page     int       `json:"page"`
	Pages    int       `json:"pages"`
	Limit    int       `json:"limit"`
	Query    struct {
		Q       string `json:"q"`              // what the shopper typed
		Used    string `json:"used,omitempty"` // what was searched, when it differs (typo fixed)
		Relaxed bool   `json:"relaxed"`        // true: no product matched every word, showing partial matches
	} `json:"query"`
	Facets *Facets `json:"facets,omitempty"`
}

func (s *Server) searchListings(ctx context.Context, f searchFilters, withFacets bool) (*searchResult, error) {
	tax, err := s.loadTaxonomy(ctx)
	if err != nil {
		return nil, err
	}
	pq, err := s.parseQuery(ctx, f.Q)
	if err != nil {
		return nil, err
	}
	res := &searchResult{Products: []Product{}, Limit: f.Limit}
	res.Query.Q = f.Q
	if pq.corrected {
		res.Query.Used = pq.text()
	}
	attrOf := tax.attrOf()

	count := func(tsquery string) (int, error) {
		sql, args := buildSQL(part(`SELECT count(*)`+productJoins+` WHERE true`), f.conds(tsquery, attrOf), "", part(""))
		var n int
		return n, s.pool.QueryRow(ctx, sql, args...).Scan(&n)
	}
	tsquery := ""
	if len(pq.tokens) > 0 {
		tsquery = pq.tsquery(true)
	}
	if res.Total, err = count(tsquery); err != nil {
		return nil, err
	}
	// nothing matches every word: show what matches any of them
	if res.Total == 0 && len(pq.tokens) > 1 {
		relaxed := pq.tsquery(false)
		n, err := count(relaxed)
		if err != nil {
			return nil, err
		}
		if n > 0 {
			tsquery, res.Total, res.Query.Relaxed = relaxed, n, true
		}
	}

	res.Pages = max(1, int(math.Ceil(float64(res.Total)/float64(f.Limit))))
	res.Page = min(f.Page, res.Pages)
	conds := f.conds(tsquery, attrOf)
	tail := f.orderBy(tsquery)
	tail.sql += ` LIMIT ? OFFSET ?`
	tail.args = append(tail.args, f.Limit, (res.Page-1)*f.Limit)
	sql, args := buildSQL(part(productQuery), conds, "", tail)
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		p, err := scanProduct(rows)
		if err != nil {
			return nil, err
		}
		res.Products = append(res.Products, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()

	if withFacets {
		if err := s.countFacets(ctx, tax, conds); err != nil {
			return nil, err
		}
		res.Facets = tax
	}
	return res, nil
}

func (s *Server) getSearch(w http.ResponseWriter, r *http.Request) {
	f := readSearchFilters(r)
	res, err := s.searchListings(r.Context(), f, true)
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	// remember how many products a plain search found: terms with 0 results
	// are gaps in the catalog (admin "Pencarian" tab). The count itself is
	// bumped by the frontend's "search" track event, once per submitted search.
	if term := strings.Join(searchTokens(f.Q), " "); term != "" && !f.filtered() && res.Page == 1 {
		_, _ = s.pool.Exec(r.Context(),
			`INSERT INTO search_terms (term, count, results) VALUES ($1, 0, $2)
			 ON CONFLICT (term) DO UPDATE SET results = $2, last_at = now()`, term, res.Total)
	}
	writeJSON(w, http.StatusOK, res)
}

// getTaxonomy: categories, product types, attributes and colours with the
// number of active products in each (GET /api/taxonomy).
func (s *Server) getTaxonomy(w http.ResponseWriter, r *http.Request) {
	tax, err := s.loadTaxonomy(r.Context())
	if err == nil {
		err = s.countFacets(r.Context(), tax, searchFilters{}.conds("", nil))
	}
	if err != nil {
		errJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, tax)
}

// ---- autocomplete ----------------------------------------------------------

type SuggestFilter struct {
	Kind  string `json:"kind"` // attr | category | type
	ID    string `json:"id"`
	Label string `json:"label"`
	Emoji string `json:"emoji,omitempty"`
	Group string `json:"group"` // "Tema", "Kategori", "Produk"
}

type SuggestCreator struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Avatar string `json:"avatar"`
}

type suggestion struct {
	Terms    []string         `json:"terms"` // query completions, popular first
	Filters  []SuggestFilter  `json:"filters"`
	Creators []SuggestCreator `json:"creators"`
	Products []Product        `json:"products"`
}

// getSuggest answers as the shopper types. Without a query it returns the
// most popular searches.
func (s *Server) getSuggest(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := suggestion{Terms: []string{}, Filters: []SuggestFilter{}, Creators: []SuggestCreator{}, Products: []Product{}}
	strs := func(sql string, args ...any) ([]string, error) {
		rows, err := s.pool.Query(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		list := []string{}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		return list, rows.Err()
	}
	fail := func(err error) { errJSON(w, http.StatusInternalServerError, err.Error()) }

	raw := strings.TrimSpace(r.URL.Query().Get("q"))
	if utf8.RuneCountInString(raw) > 60 {
		raw = string([]rune(raw)[:60])
	}
	tokens := searchTokens(raw)
	if len(tokens) == 0 {
		terms, err := strs(`SELECT term FROM search_terms WHERE results IS DISTINCT FROM 0 AND count > 0 ORDER BY count DESC, term LIMIT 6`)
		if err != nil {
			fail(err)
			return
		}
		out.Terms = terms
		writeJSON(w, http.StatusOK, out)
		return
	}
	typed := strings.Join(tokens, " ") // letters, digits and spaces: safe inside LIKE

	// 1. popular searches that start with (or contain a word starting with) the input
	terms, err := strs(`
		SELECT term FROM search_terms
		WHERE results IS DISTINCT FROM 0 AND count > 0 AND (term LIKE $1 || '%' OR term LIKE '% ' || $1 || '%')
		ORDER BY (term LIKE $1 || '%') DESC, count DESC, term LIMIT 4`, typed)
	if err != nil {
		fail(err)
		return
	}
	// 2. complete the last word from the catalog vocabulary
	last := tokens[len(tokens)-1]
	words, err := strs(`SELECT word FROM search_words WHERE word LIKE $1 || '%' AND word <> $1 ORDER BY ndoc DESC, word LIMIT 4`, last)
	if err != nil {
		fail(err)
		return
	}
	seen := map[string]bool{typed: true}
	for _, t := range terms {
		if !seen[t] {
			seen[t] = true
			out.Terms = append(out.Terms, t)
		}
	}
	for _, word := range words {
		t := strings.Join(append(append([]string{}, tokens[:len(tokens)-1]...), word), " ")
		if !seen[t] && len(out.Terms) < 6 {
			seen[t] = true
			out.Terms = append(out.Terms, t)
		}
	}

	// 3. shortcuts to a filter whose name starts with the input
	rows, err := s.pool.Query(ctx, `
		SELECT kind, id, label, emoji, grp FROM (
			SELECT 'attr' AS kind, av.id, av.label, av.emoji, a.label AS grp, 2 AS ord FROM attribute_values av JOIN attributes a ON a.id = av.attribute_id
			UNION ALL SELECT 'category', id, label, emoji, 'Kategori', 1 FROM categories
			UNION ALL SELECT 'type', id, label, '', 'Produk', 0 FROM product_types
		) x WHERE lower(label) LIKE $1 || '%' ORDER BY ord, label LIMIT 4`, typed)
	if err != nil {
		fail(err)
		return
	}
	for rows.Next() {
		var f SuggestFilter
		if err := rows.Scan(&f.Kind, &f.ID, &f.Label, &f.Emoji, &f.Group); err != nil {
			rows.Close()
			fail(err)
			return
		}
		out.Filters = append(out.Filters, f)
	}
	rows.Close()

	// 4. creators with something on sale
	rows, err = s.pool.Query(ctx, `
		SELECT dr.id, dr.name, dr.avatar_uri FROM designers dr
		WHERE lower(dr.name) LIKE '%' || $1 || '%'
		  AND EXISTS (SELECT 1 FROM designs d JOIN listings l ON l.design_id = d.id AND l.active WHERE d.designer_id = dr.id)
		ORDER BY dr.followers DESC, dr.name LIMIT 3`, typed)
	if err != nil {
		fail(err)
		return
	}
	for rows.Next() {
		var c SuggestCreator
		if err := rows.Scan(&c.ID, &c.Name, &c.Avatar); err != nil {
			rows.Close()
			fail(err)
			return
		}
		out.Creators = append(out.Creators, c)
	}
	rows.Close()

	// 5. best matching products
	res, err := s.searchListings(ctx, searchFilters{Q: raw, Page: 1, Limit: 4}, false)
	if err != nil {
		fail(err)
		return
	}
	out.Products = res.Products
	writeJSON(w, http.StatusOK, out)
}
