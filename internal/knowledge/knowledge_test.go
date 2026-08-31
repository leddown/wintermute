package knowledge

import (
	"context"
	"strings"
	"testing"

	"wintermute/internal/store/storetest"
	"wintermute/internal/tool"
)

func newTestService(t *testing.T) *Service {
	t.Helper()
	st := storetest.New(t)
	return NewService(NewStore(st.DB()))
}

func mustAgent(t *testing.T, svc *Service, name string, sources ...string) *Agent {
	t.Helper()
	agent, err := svc.CreateAgent(context.Background(), &Agent{Name: name, Sources: sources})
	if err != nil {
		t.Fatalf("CreateAgent(%q): %v", name, err)
	}
	return agent
}

func TestAgentValidate(t *testing.T) {
	tests := []struct {
		name    string
		agent   Agent
		wantID  string
		wantErr string
	}{
		{
			name:   "id is derived from the name",
			agent:  Agent{Name: "Acme Bank — GRC"},
			wantID: "acme-bank-grc",
		},
		{
			name:   "sources are normalised and deduplicated",
			agent:  Agent{Name: "x", Sources: []string{"web", "documents", "web"}},
			wantID: "x",
		},
		{name: "a name is required", agent: Agent{}, wantErr: "a name is required"},
		{
			name:    "an unknown source is refused",
			agent:   Agent{Name: "x", Sources: []string{"filesystem"}},
			wantErr: `unknown source "filesystem"`,
		},
		{
			name:    "an explicit id must be a slug",
			agent:   Agent{ID: "Not A Slug", Name: "x"},
			wantErr: "must be lowercase letters",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			agent := tc.agent
			err := agent.Validate()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Validate() = %v, want it to contain %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Validate(): %v", err)
			}
			if agent.ID != tc.wantID {
				t.Errorf("ID = %q, want %q", agent.ID, tc.wantID)
			}
			for i := 1; i < len(agent.Sources); i++ {
				if agent.Sources[i-1] >= agent.Sources[i] {
					t.Errorf("sources are not sorted and deduplicated: %v", agent.Sources)
				}
			}
		})
	}
}

func TestAgentHas(t *testing.T) {
	agent := &Agent{Sources: []string{SourceDocuments, SourceGRC}}
	if !agent.Has(SourceGRC) || !agent.Has(SourceDocuments) {
		t.Error("Has() missed a declared source")
	}
	if agent.Has(SourceWeb) {
		t.Error("Has() reported an undeclared source")
	}
	// A nil agent is the unscoped assistant, which has nothing.
	var none *Agent
	if none.Has(SourceDocuments) {
		t.Error("a nil agent reported a source")
	}
}

// Chunking is what search retrieves, so the boundaries matter more than they
// look: a chunk that loses its heading is a chunk that no longer answers the
// question it is the answer to.
func TestChunkText(t *testing.T) {
	text := `# Acme Policy

Intro paragraph that sets out the scope of this document in enough words to
stand as its own passage rather than being folded into the next one.

## 7. Network segmentation

The cardholder data environment is segmented from the corporate network by
firewalls that deny all traffic by default.

Article 17 Incident reporting

A major incident must be reported within 24 hours of detection, and to the
affected customers within seventy-two hours of the same moment.
`
	chunks := ChunkText(text)
	if len(chunks) < 3 {
		t.Fatalf("got %d chunks, want at least 3: %+v", len(chunks), chunks)
	}

	headings := make([]string, 0, len(chunks))
	for i, c := range chunks {
		headings = append(headings, c.Heading)
		if c.Ordinal != i {
			t.Errorf("chunk %d has ordinal %d", i, c.Ordinal)
		}
	}
	for _, want := range []string{"Acme Policy", "7. Network segmentation", "Article 17 Incident reporting"} {
		if !containsString(headings, want) {
			t.Errorf("headings %q are missing %q", headings, want)
		}
	}

	var segmentation Chunk
	for _, c := range chunks {
		if c.Heading == "7. Network segmentation" {
			segmentation = c
		}
	}
	if !strings.Contains(segmentation.Body, "deny all traffic by default") {
		t.Errorf("the segmentation chunk lost its body: %q", segmentation.Body)
	}
	if strings.Contains(segmentation.Body, "major incident") {
		t.Error("the segmentation chunk swallowed the next section")
	}
}

// A numbered sentence is not a heading. Treating one as a boundary shatters a
// paragraph into fragments that retrieve badly.
func TestChunkTextIgnoresNumberedProse(t *testing.T) {
	text := "Some preamble.\n\n" +
		"1. The controller shall implement appropriate technical measures to ensure a level of " +
		"security appropriate to the risk, taking into account the state of the art.\n"
	chunks := ChunkText(text)
	for _, c := range chunks {
		if strings.HasPrefix(c.Heading, "1.") {
			t.Errorf("a numbered sentence became a heading: %q", c.Heading)
		}
	}
}

func TestSearchRanksAndReportsTerms(t *testing.T) {
	chunks := []Chunk{
		{DocumentID: 1, Ordinal: 0, Heading: "Access control", Body: "Privileged access requires multi-factor authentication."},
		{DocumentID: 1, Ordinal: 1, Heading: "Network segmentation", Body: "The cardholder environment is segmented by firewalls."},
		{DocumentID: 1, Ordinal: 2, Heading: "Incident reporting", Body: "Report within 24 hours."},
	}

	hits := Search("network segmentation firewalls", chunks, 5)
	if len(hits) == 0 {
		t.Fatal("Search returned nothing")
	}
	if hits[0].Chunk.Ordinal != 1 {
		t.Errorf("best hit is chunk %d, want the segmentation one", hits[0].Chunk.Ordinal)
	}
	if !containsString(hits[0].Terms, "segmentation") || !containsString(hits[0].Terms, "firewalls") {
		t.Errorf("matched terms = %v, want the query's terms", hits[0].Terms)
	}
	// A query of only stopwords is not a search.
	if got := Search("the and of", chunks, 5); len(got) != 0 {
		t.Errorf("a stopword-only query returned %d hits", len(got))
	}
	// The heading is scored, so a word only in the heading still retrieves.
	if got := Search("access", chunks, 5); len(got) == 0 || got[0].Chunk.Ordinal != 0 {
		t.Errorf("a heading-only match did not retrieve: %+v", got)
	}
}

func TestTokenizeKeepsIdentifiers(t *testing.T) {
	got := Tokenize("Does AC-2 cover multi-factor auth for the CDE?")
	for _, want := range []string{"ac-2", "cover", "multi-factor", "auth", "cde"} {
		if !containsString(got, want) {
			t.Errorf("tokens %v are missing %q", got, want)
		}
	}
	for _, unwanted := range []string{"the", "for", "does"} {
		if containsString(got, unwanted) {
			t.Errorf("tokens %v kept the stopword %q", got, unwanted)
		}
	}
}

func TestExtractRejectsWhatItCannotRead(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		body     []byte
		wantErr  string
	}{
		{"empty", "a.txt", nil, "empty"},
		{"oversized", "a.txt", make([]byte, MaxDocumentBytes+1), "the limit is"},
		{"unsupported", "a.xlsx", []byte("x"), "unsupported document type"},
		{"not utf-8", "a.txt", []byte{0xff, 0xfe, 0x00, 0x01}, "not valid UTF-8"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Extract(tc.filename, "", tc.body)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Extract = %v, want it to contain %q", err, tc.wantErr)
			}
		})
	}

	got, err := Extract("policy.md", "", []byte("# Title\n\nBody text.\n"))
	if err != nil {
		t.Fatalf("Extract markdown: %v", err)
	}
	if got.Via != "direct read" || !strings.Contains(got.Text, "Body text.") {
		t.Errorf("extraction = %+v", got)
	}
}

func TestUploadAndSearchLibrary(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	agent := mustAgent(t, svc, "Acme Bank", SourceDocuments)

	body := []byte("# Acme\n\n## Network segmentation\n\nThe CDE is segmented from the corporate " +
		"network by firewalls that deny all traffic by default.\n")
	doc, err := svc.Upload(ctx, UploadInput{
		AgentID: agent.ID, Filename: "acme.md", Body: body, Title: "Acme Policy",
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if doc.ChunkCount == 0 {
		t.Fatal("the document stored no chunks, so it can never be retrieved")
	}

	// The same file twice in one library is a mistake worth naming.
	if _, err := svc.Upload(ctx, UploadInput{AgentID: agent.ID, Filename: "acme.md", Body: body}); err == nil {
		t.Error("Upload accepted a duplicate")
	}

	hits, err := svc.SearchLibrary(ctx, agent.ID, "segmentation firewalls", 5)
	if err != nil {
		t.Fatalf("SearchLibrary: %v", err)
	}
	if len(hits) == 0 || !strings.Contains(hits[0].Chunk.Body, "deny all traffic") {
		t.Fatalf("search did not find the passage: %+v", hits)
	}
	if hits[0].Chunk.Title != "Acme Policy" {
		t.Errorf("hit lost its document title: %q", hits[0].Chunk.Title)
	}
}

// The boundary this package exists to draw: one agent cannot read another's
// library, whatever document id it names.
func TestLibrariesAreSeparate(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	acme := mustAgent(t, svc, "Acme", SourceDocuments)
	other := mustAgent(t, svc, "Other", SourceDocuments)

	doc, err := svc.Upload(ctx, UploadInput{
		AgentID: acme.ID, Filename: "secret.md",
		Body: []byte("# Acme\n\nThe merger completes in March.\n"),
	})
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}

	hits, err := svc.SearchLibrary(ctx, other.ID, "merger March", 5)
	if err != nil {
		t.Fatalf("SearchLibrary: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("the other agent's search reached this library: %+v", hits)
	}

	if _, _, err := svc.ReadDocument(ctx, other.ID, doc.ID, 0, 3); err == nil {
		t.Error("the other agent read the document by naming its id")
	}
}

// Deleting an agent must not take conversations with it; a session simply
// becomes unscoped.
func TestDeleteAgentLeavesSessionsUsable(t *testing.T) {
	ctx := context.Background()
	svc := newTestService(t)
	agent := mustAgent(t, svc, "Temp", SourceDocuments)

	if err := svc.DeleteAgent(ctx, agent.ID); err != nil {
		t.Fatalf("DeleteAgent: %v", err)
	}
	// Lookup of a deleted agent is "no agent", not an error: a stored session
	// pinned to it must keep answering.
	got, err := svc.Lookup(ctx, agent.ID)
	if err != nil {
		t.Fatalf("Lookup after delete: %v", err)
	}
	if got != nil {
		t.Errorf("Lookup returned %+v after deletion", got)
	}
}

// RegisterFor is what puts the document tools in front of a model, and it must
// stay silent for an agent that has no document source.
func TestRegisterForRespectsSources(t *testing.T) {
	svc := newTestService(t)

	withDocs := tool.NewRegistry()
	if err := RegisterFor(withDocs, svc, &Agent{ID: "a", Sources: []string{SourceDocuments}}); err != nil {
		t.Fatalf("RegisterFor: %v", err)
	}
	if len(withDocs.Definitions()) != 3 {
		t.Errorf("registered %d tools, want 3", len(withDocs.Definitions()))
	}

	without := tool.NewRegistry()
	if err := RegisterFor(without, svc, &Agent{ID: "a", Sources: []string{SourceGRC}}); err != nil {
		t.Fatalf("RegisterFor: %v", err)
	}
	if n := len(without.Definitions()); n != 0 {
		t.Errorf("registered %d document tools for an agent without the source", n)
	}

	none := tool.NewRegistry()
	if err := RegisterFor(none, svc, nil); err != nil {
		t.Fatalf("RegisterFor(nil): %v", err)
	}
	if n := len(none.Definitions()); n != 0 {
		t.Errorf("registered %d tools for no agent", n)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}
