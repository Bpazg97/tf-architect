package state

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Phase string

const (
	PhaseIngest   Phase = "ingest"
	PhaseAnalyze  Phase = "analyze"
	PhaseClarify  Phase = "clarify"
	PhaseGenerate Phase = "generate"
	PhaseValidate Phase = "validate"
	PhaseDocument Phase = "document"
	PhaseDone     Phase = "done"
	PhaseFailed   Phase = "failed"
)

var phaseOrder = map[Phase]int{
	PhaseIngest:   0,
	PhaseAnalyze:  1,
	PhaseClarify:  2,
	PhaseGenerate: 3,
	PhaseValidate: 4,
	PhaseDocument: 5,
	PhaseDone:     6,
	PhaseFailed:   -1,
}

func (p Phase) Before(other Phase) bool {
	return phaseOrder[p] < phaseOrder[other]
}

func (p Phase) AtOrBefore(other Phase) bool {
	return phaseOrder[p] <= phaseOrder[other]
}

type Checkpoint struct {
	Phase        Phase     `json:"phase"`
	ClarifyRound int       `json:"clarify_round,omitempty"`
	GenerateStep string    `json:"generate_step,omitempty"`
	CompletedAt  time.Time `json:"completed_at"`
}

type ClarificationRound struct {
	Round     int            `json:"round"`
	Questions []string       `json:"questions"`
	Answers   map[int]string `json:"answers"`
}

type ValidationRun struct {
	RunAt   time.Time       `json:"run_at"`
	Passed  bool            `json:"passed"`
	Results map[string]bool `json:"results"`
	Errors  []string        `json:"errors,omitempty"`
}

type Session struct {
	DocPath   string    `json:"doc_path"`
	DocHash   string    `json:"doc_hash"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`

	CurrentPhase   Phase      `json:"current_phase"`
	LastCheckpoint Checkpoint `json:"last_checkpoint"`

	ClaudeSessionID string `json:"claude_session_id,omitempty"`

	DocMarkdown    string               `json:"doc_markdown"`
	AnalysisJSON   string               `json:"analysis_json"`
	Clarifications []ClarificationRound `json:"clarifications"`
	GeneratedFiles []string             `json:"generated_files"`
	ValidationRuns []ValidationRun      `json:"validation_runs"`

	PreviousDocHash string `json:"previous_doc_hash,omitempty"`
	DiffSummary     string `json:"diff_summary,omitempty"`
	MaskMapPath     string `json:"mask_map_path,omitempty"`
}

type Manager struct {
	BaseDir string
}

func NewManager(projectDir string) *Manager {
	return &Manager{
		BaseDir: filepath.Join(projectDir, ".tf-architect", "sessions"),
	}
}

func (m *Manager) sessionDir(docHash string) string {
	return filepath.Join(m.BaseDir, docHash)
}

func (m *Manager) sessionPath(docHash string) string {
	return filepath.Join(m.sessionDir(docHash), "session.json")
}

func (m *Manager) Load(docHash string) (*Session, error) {
	data, err := os.ReadFile(m.sessionPath(docHash))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s Session
	return &s, json.Unmarshal(data, &s)
}

func (m *Manager) Save(s *Session) error {
	dir := m.sessionDir(s.DocHash)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	s.UpdatedAt = time.Now()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.sessionPath(s.DocHash), data, 0644)
}

func (m *Manager) Advance(s *Session, phase Phase, checkpoint Checkpoint) error {
	s.CurrentPhase = phase
	checkpoint.Phase = phase
	checkpoint.CompletedAt = time.Now()
	s.LastCheckpoint = checkpoint
	return m.Save(s)
}

// FindByDocPath returns the most recently updated session for a given doc path.
func (m *Manager) FindByDocPath(docPath string) (*Session, error) {
	entries, err := os.ReadDir(m.BaseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var latest *Session
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		s, err := m.Load(entry.Name())
		if err != nil || s == nil {
			continue
		}
		if s.DocPath == docPath {
			if latest == nil || s.UpdatedAt.After(latest.UpdatedAt) {
				latest = s
			}
		}
	}
	return latest, nil
}

// MaskMapPath returns the absolute path to mask.json for a session.
func (m *Manager) MaskMapPath(docHash string) string {
	return filepath.Join(m.sessionDir(docHash), "mask.json")
}

// HashDoc returns the SHA256 hex digest of the file at path.
func HashDoc(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum), nil
}

// MaskMapPath returns the absolute path to mask.json for a session.
func (m *Manager) MaskMapPath(docHash string) string {
	return filepath.Join(m.sessionDir(docHash), "mask.json")
}
