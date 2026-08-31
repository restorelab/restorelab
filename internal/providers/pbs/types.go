package pbs

import "encoding/json"

// Datastore is a PBS datastore as returned by GET /admin/datastore.
type Datastore struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Comment string `json:"comment,omitempty"`
}

// datastoreEntry is the raw shape of one element in the
// GET /admin/datastore response. PBS nests the config under "store" but also
// duplicates the name at the top level depending on version, so both are
// tolerated.
type datastoreEntry struct {
	Store   string `json:"store"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	Comment string `json:"comment"`
}

// snapshotVerification mirrors the "verification" object embedded in a
// snapshot listing. PBS omits the field entirely when a snapshot was never
// verified, so it must stay a pointer at the call site.
type snapshotVerification struct {
	State   string     `json:"state"`
	UPID    string     `json:"upid,omitempty"`
	Started jsonNumber `json:"starttime,omitempty"`
}

// snapshotFile describes one file within a snapshot (the disk images, the
// client log, the index...). Only CryptMode matters to RestoreLab today.
type snapshotFile struct {
	Filename  string     `json:"filename"`
	CryptMode string     `json:"crypt-mode,omitempty"`
	Size      jsonNumber `json:"size,omitempty"`
}

// snapshotEntry is the raw shape of one element in the
// GET /admin/datastore/{store}/snapshots response.
//
// PBS is inconsistent about whether numeric fields are encoded as JSON
// numbers or JSON strings depending on version and field, so every numeric
// field goes through jsonNumber, a tolerant decoder (see jsonnumber.go).
type snapshotEntry struct {
	BackupType   string                `json:"backup-type"`
	BackupID     string                `json:"backup-id"`
	BackupTime   jsonNumber            `json:"backup-time"`
	Size         jsonNumber            `json:"size"`
	Protected    bool                  `json:"protected"`
	Comment      string                `json:"comment"`
	Verification *snapshotVerification `json:"verification"`
	Files        []snapshotFile        `json:"files"`
}

// versionResponse is the payload of GET /version, used only as a reachability
// probe by Ping.
type versionResponse struct {
	Version string `json:"version"`
	Release string `json:"release"`
}

// envelope is the {"data": ...} wrapper every PBS API response uses.
type envelope struct {
	Data json.RawMessage `json:"data"`
}
