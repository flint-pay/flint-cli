package opindex

import (
	"math"
	"regexp"
	"sort"
	"strings"
)

var (
	searchWord      = regexp.MustCompile(`[a-z0-9]+`)
	searchCamel     = regexp.MustCompile(`([a-z0-9])([A-Z])`)
	searchParameter = regexp.MustCompile(`\{[^}]*\}`)
	searchStop      = wordSet(strings.Fields("a an the to for of in on my me i and or with by from how do can what is are all this that it be into at as new"))
	searchVerbs     = map[string]string{
		"add": "create", "make": "create", "issue": "create", "start": "create",
		"show": "get", "fetch": "get", "retrieve": "get", "read": "get", "look": "get",
		"find": "list", "search": "list", "browse": "list", "change": "update",
		"edit": "update", "modify": "update", "set": "update", "rename": "update",
		"remove": "delete", "erase": "delete", "stop": "cancel",
	}
	searchNouns = map[string]string{
		"charge": "payment-intent", "card": "payment-method", "shipping": "delivery",
		"coupon": "promotion-code", "discount": "promotion", "whoami": "auth",
		"credential": "auth", "stock": "inventory-level", "sku": "variant",
	}
)

type searchField struct {
	words  map[string]bool
	weight float64
}

type searchDocument struct {
	entry    Entry
	fields   []searchField
	segments [][]string
}

// Search ranks the offline index, applying exposure and kind filters before the
// result limit. The total includes all matching entries permitted by the filter.
func (ix *Index) Search(query string, filter Filter) ([]Hit, int) {
	q := strings.ToLower(strings.TrimSpace(query))
	tokens := searchTokens(q)
	docs := make([]searchDocument, 0, len(ix.Entries))
	df := make(map[string]int)
	for _, entry := range ix.Entries {
		doc := newSearchDocument(entry)
		seen := make(map[string]bool)
		for _, field := range doc.fields {
			for word := range field.words {
				seen[word] = true
			}
		}
		for word := range seen {
			df[word]++
		}
		docs = append(docs, doc)
	}
	idf := func(word string) float64 { return math.Log1p(float64(len(docs)) / float64(1+df[word])) }
	type rankedHit struct {
		Hit
		priority int
	}
	ranked := make([]rankedHit, 0)
	methodPath := strings.Fields(strings.TrimSpace(query))
	for _, doc := range docs {
		e := doc.entry
		if filter.Exposed != nil && !filter.Exposed(e) {
			continue
		}
		if filter.Kind != "" && (e.Type != "operation" || e.Kind != filter.Kind) {
			continue
		}
		score, priority := scoreSearchDocument(doc, tokens, idf), 0
		if e.Type == "operation" && q != "" && (strings.ToLower(e.ID) == q || strings.HasPrefix(strings.ToLower(e.ID), q+".")) {
			score, priority = 100, 1
		}
		if e.Type == "operation" && len(methodPath) == 2 && e.Method != "" && e.Path != "" &&
			strings.EqualFold(e.Method, methodPath[0]) && e.Path == methodPath[1] {
			score, priority = 200, 2
		}
		if e.Type == "event" {
			factor := 0.6
			for _, t := range tokens {
				if t == "event" || t == "webhook" {
					factor = 1.2
				}
			}
			score *= factor
		}
		if score > 0 {
			ranked = append(ranked, rankedHit{Hit{Entry: e, Score: score}, priority})
		}
	}
	sort.Slice(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.priority != b.priority {
			return a.priority > b.priority
		}
		if x, y := math.Round(a.Score*1000), math.Round(b.Score*1000); x != y {
			return x > y
		}
		if x, y := a.Type == "operation", b.Type == "operation"; x != y {
			return x
		}
		if x, y := strings.Count(a.ID, "."), strings.Count(b.ID, "."); x != y {
			return x < y
		}
		if x, y := searchVerbRank(a.Entry), searchVerbRank(b.Entry); x != y {
			return x < y
		}
		return a.ID < b.ID
	})
	total, limit := len(ranked), filter.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > total {
		limit = total
	}
	hits := make([]Hit, limit)
	for i := range hits {
		hits[i] = ranked[i].Hit
	}
	return hits, total
}

func newSearchDocument(e Entry) searchDocument {
	d := searchDocument{entry: e}
	segments := strings.Split(e.ID, ".")
	var nouns, last, verb []string
	switch e.Type {
	case "event":
		nouns = searchWords(segments[0])
		last = nouns
		verb = searchWords(strings.Join(segments[1:], " "))
	case "topic":
		last = searchWords(e.ID)
	default:
		resources := segments
		if len(segments) > 1 {
			resources = segments[:len(segments)-1]
			verb = searchWords(segments[len(segments)-1])
		}
		for _, segment := range resources {
			last = searchWords(segment)
			nouns = append(nouns, last...)
			d.segments = append(d.segments, last)
		}
	}
	title := searchWords(e.Title)
	if e.Type == "event" {
		title = append(title, "event", "webhook")
	}
	description := searchWords(searchFirstSentence(e.Description))
	for _, alias := range e.Aliases {
		description = append(description, searchWords(alias)...)
		description = append(description, strings.Join(searchWord.FindAllString(strings.ToLower(alias), -1), ""))
	}
	if e.ID == "auth.status" {
		description = append(description, "whoami", "credential", "context")
	}
	d.fields = []searchField{
		{wordSet(last), 4}, {wordSet(nouns), 2.5}, {wordSet(verb), 3}, {wordSet(title), 1.5},
		{wordSet(searchWords(searchCamel.ReplaceAllString(e.OperationID, "${1} ${2}"))), 1},
		{wordSet(searchWords(searchParameter.ReplaceAllString(e.Path, ""))), 1}, {wordSet(description), 0.6},
	}
	return d
}

func scoreSearchDocument(doc searchDocument, tokens []string, idf func(string) float64) float64 {
	queryWords := wordSet(tokens)
	for _, token := range tokens {
		for _, word := range strings.Split(token, "-") {
			queryWords[word] = true
		}
	}
	total := 0.0
	for i, segment := range doc.segments {
		all, bonus := len(segment) > 0, 0.0
		for _, word := range segment {
			all = all && queryWords[word]
			bonus += 3 * idf(word)
		}
		if all {
			total += bonus
			if i == len(doc.segments)-1 {
				total++
			}
		}
	}
	matched := 0
	for _, token := range tokens {
		// Hyphenated domain synonyms contribute through resource segments,
		// including queries whose only match is the synonym (for example stock).
		if strings.Contains(token, "-") {
			continue
		}
		best := 0.0
		for _, field := range doc.fields {
			if field.words[token] {
				best = math.Max(best, field.weight)
				continue
			}
			if len(token) >= 4 {
				for word := range field.words {
					if strings.HasPrefix(word, token) {
						best = math.Max(best, field.weight*0.5)
					}
				}
			}
		}
		if best > 0 {
			matched++
		}
		total += best * idf(token)
	}
	if matched == len(tokens) {
		total *= 1.5
	}
	return total
}

func searchTokens(query string) []string {
	tokens := make([]string, 0)
	for _, word := range searchWord.FindAllString(query, -1) {
		if searchStop[word] {
			continue
		}
		if synonym := searchVerbs[word]; synonym != "" {
			word = synonym
		}
		tokens = append(tokens, searchSingular(word))
	}
	seen := wordSet(tokens)
	addNoun := func(word string) {
		if noun := searchNouns[word]; noun != "" && !seen[noun] {
			tokens = append(tokens, noun)
			seen[noun] = true
		}
	}
	for _, token := range tokens {
		addNoun(token)
	}
	joined := strings.Join(searchWord.FindAllString(query, -1), "")
	addNoun(joined)
	// Joined aliases are searchable even when their constituent words are stopwords.
	if joined != "" && !seen[joined] && len(searchWord.FindAllString(query, -1)) > 1 {
		if joined == "whoami" {
			tokens = append(tokens, joined)
		}
	}
	return tokens
}

func searchWords(text string) []string {
	words := make([]string, 0)
	for _, word := range searchWord.FindAllString(strings.ToLower(text), -1) {
		if !searchStop[word] {
			words = append(words, searchSingular(word))
		}
	}
	return words
}

func searchSingular(word string) string {
	switch {
	case len(word) > 4 && strings.HasSuffix(word, "ies"):
		return word[:len(word)-3] + "y"
	case strings.HasSuffix(word, "sses"), strings.HasSuffix(word, "xes"), strings.HasSuffix(word, "ches"), strings.HasSuffix(word, "shes"):
		return word[:len(word)-2]
	case len(word) > 3 && strings.HasSuffix(word, "s") && !strings.HasSuffix(word, "ss"):
		return word[:len(word)-1]
	}
	return word
}

func searchFirstSentence(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	for offset := 0; offset < len(text); {
		i := strings.Index(text[offset:], ". ")
		if i < 0 {
			break
		}
		i += offset
		if !strings.HasSuffix(text[:i], "e.g") && !strings.HasSuffix(text[:i], "i.e") {
			return text[:i+1]
		}
		offset = i + 2
	}
	return text
}

func wordSet(words []string) map[string]bool {
	set := make(map[string]bool, len(words))
	for _, word := range words {
		set[word] = true
	}
	return set
}

func searchVerbRank(e Entry) int {
	if e.Type != "operation" {
		return 5
	}
	parts := strings.Split(e.ID, ".")
	switch parts[len(parts)-1] {
	case "list":
		return 0
	case "get":
		return 1
	case "create":
		return 2
	case "update":
		return 3
	default:
		return 4
	}
}
