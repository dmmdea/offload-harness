package ledger

// SessionShare is one session's card tokens split by WHERE they ran (register D-101, ADR 0085).
//
// It is the card-side placement split: of the tokens the session's own jobs put through the cards, how many ran
// on a fleet node (RemoteTokens) and how many on this box (LocalTokens). It is NOT the share gate's figure,
// cards over cards plus cloud, which needs the session's transcript for the cloud term and stays in the hook.
type SessionShare struct {
	// Session is the origin_session of the rows; "" is every row that carries none (unattributed), never merged
	// into a named session.
	Session string `json:"session"`
	// Rows is the number of jobs the session asked for that this ledger holds (a dispatch marker and an inner
	// row are not jobs).
	Rows int `json:"rows"`
	// CardsTokens is the card work of those jobs, RemoteTokens + LocalTokens.
	CardsTokens int `json:"cards_tokens"`
	// RemoteTokens is the part that ran on a fleet node; LocalTokens the part that ran on this box.
	RemoteTokens int `json:"remote_tokens"`
	LocalTokens  int `json:"local_tokens"`
	// RemoteShare is RemoteTokens over CardsTokens, 0 when the session did no card work.
	RemoteShare float64 `json:"remote_share"`
	// ServedRows and ServedTokens are the jobs this box ran FOR the session as a fleet node (door "fleet"),
	// on this box's own ledger. They are never part of the session's own totals above.
	ServedRows   int `json:"served_rows"`
	ServedTokens int `json:"served_tokens"`
}

// SessionShares groups entries by session (see SessionShare).
func SessionShares(entries []Entry) []SessionShare {
	return nil
}

// SessionSharesFile reads the ledger at path and groups its rows by session (see SessionShares).
func SessionSharesFile(path string, since int64, includeTraffic bool) ([]SessionShare, map[string]int, error) {
	return nil, nil, nil
}
