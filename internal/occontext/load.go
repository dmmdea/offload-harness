package occontext

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The JSON shapes opencode 1.18 writes into message.data and part.data (only the fields
// read here). A field opencode drops simply reads as its zero value.
type messageData struct {
	Role       string          `json:"role"`
	Agent      string          `json:"agent"`
	ModelID    string          `json:"modelID"`
	ProviderID string          `json:"providerID"`
	Summary    json.RawMessage `json:"summary"` // true on a compaction summary; an object on user messages
	Tokens     *struct {
		Total     int `json:"total"`
		Input     int `json:"input"`
		Output    int `json:"output"`
		Reasoning int `json:"reasoning"`
		Cache     struct {
			Read  int `json:"read"`
			Write int `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Time struct {
		Created   int64 `json:"created"`
		Completed int64 `json:"completed"`
	} `json:"time"`
}

type partData struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Synthetic bool   `json:"synthetic"`
	Time      *struct {
		Start int64 `json:"start"`
	} `json:"time"`
	Tool  string `json:"tool"`
	State *struct {
		Status string          `json:"status"`
		Output json.RawMessage `json:"output"`
		Error  string          `json:"error"`
	} `json:"state"`
	Auto     bool `json:"auto"`
	Overflow bool `json:"overflow"`
}

func fromMillis(v int64) time.Time {
	if v <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(v)
}

// rawText is a JSON string's value, or the raw JSON when the value is not a string.
func rawText(r json.RawMessage) string {
	if len(r) == 0 || string(r) == "null" {
		return ""
	}
	var s string
	if err := json.Unmarshal(r, &s); err == nil {
		return s
	}
	return string(r)
}

// Load reads sessions, messages and parts from an opened copy. Rows whose data does not
// parse are counted in Store.Unparsed, never silently dropped.
func Load(db *sql.DB) (*Store, error) {
	for _, table := range []string{"session", "message", "part"} {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, fmt.Errorf("not an opencode session store: no %q table", table)
		}
	}
	s := &Store{Messages: map[string][]Message{}}

	rows, err := db.Query("SELECT id, COALESCE(parent_id, ''), title, time_created FROM session ORDER BY time_created, id")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var ss Session
		var created int64
		if err := rows.Scan(&ss.ID, &ss.ParentID, &ss.Title, &created); err != nil {
			rows.Close()
			return nil, err
		}
		ss.Created = fromMillis(created)
		s.Sessions = append(s.Sessions, ss)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	parts := map[string][]Part{}
	rows, err = db.Query("SELECT id, message_id, time_created, data FROM part ORDER BY message_id, id")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, mid, data string
		var created int64
		if err := rows.Scan(&id, &mid, &created, &data); err != nil {
			rows.Close()
			return nil, err
		}
		var pd partData
		if err := json.Unmarshal([]byte(data), &pd); err != nil {
			s.Unparsed++
			continue
		}
		p := Part{ID: id, Type: pd.Type, Created: fromMillis(created), Text: pd.Text, Synthetic: pd.Synthetic,
			Tool: pd.Tool, Auto: pd.Auto, Overflow: pd.Overflow}
		if pd.Time != nil {
			p.Start = fromMillis(pd.Time.Start)
		}
		if pd.State != nil {
			if pd.State.Status == "error" {
				p.ToolOut = pd.State.Error
			} else {
				p.ToolOut = rawText(pd.State.Output)
			}
		}
		parts[mid] = append(parts[mid], p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	rows, err = db.Query("SELECT id, session_id, time_created, data FROM message ORDER BY session_id, time_created, id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, sid, data string
		var created int64
		if err := rows.Scan(&id, &sid, &created, &data); err != nil {
			return nil, err
		}
		var md messageData
		if err := json.Unmarshal([]byte(data), &md); err != nil {
			s.Unparsed++
			continue
		}
		m := Message{ID: id, SessionID: sid, Role: md.Role, Agent: md.Agent, Provider: md.ProviderID, Model: md.ModelID,
			Created: fromMillis(md.Time.Created), Completed: fromMillis(md.Time.Completed),
			Summary: strings.TrimSpace(string(md.Summary)) == "true", Parts: parts[id]}
		if m.Created.IsZero() {
			m.Created = fromMillis(created)
		}
		if md.Tokens != nil {
			m.Tokens = Tokens{Input: md.Tokens.Input, Output: md.Tokens.Output, Reasoning: md.Tokens.Reasoning,
				CacheRead: md.Tokens.Cache.Read, CacheWrite: md.Tokens.Cache.Write, Total: md.Tokens.Total}
		}
		s.Messages[sid] = append(s.Messages[sid], m)
	}
	return s, rows.Err()
}

// ParseTime reads RFC 3339, or a local "YYYY-MM-DD[ HH:MM[:SS]]" (a T separator works too).
func ParseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized time %q (want RFC 3339, or local YYYY-MM-DD[ HH:MM[:SS]])", s)
}

// ParseCutoff parses "[MODEL=]TIME": TIME alone applies to every model.
func ParseCutoff(s string) (Cutoff, error) {
	var c Cutoff
	if i := strings.LastIndex(s, "="); i >= 0 {
		c.Model = strings.TrimSpace(s[:i])
		s = s[i+1:]
		if c.Model == "" {
			return c, fmt.Errorf("cutoff %q: empty model before '='", s)
		}
	}
	t, err := ParseTime(s)
	if err != nil {
		return c, err
	}
	c.Since = t
	return c, nil
}
