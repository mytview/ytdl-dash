// Package model holds the JSON shapes shared by the agent and the dashboard. The dashboard page reads
// exactly these shapes, so a change here is a change to the page.
package model

// Channel is one subscription as the agent sees it: where it comes from (the subscriptions file), what is on
// disk (the archive and the video files) and what the last run said about it.
type Channel struct {
	Name     string   `json:"name"`
	URL      string   `json:"url"`
	Instance string   `json:"instance"`
	Preset   string   `json:"preset"`            // first preset of the chain, e.g. recent or keep
	Presets  []string `json:"presets,omitempty"` // the whole chain, outermost first
	Group    string   `json:"group,omitempty"`   // first "= value" override on the way down (a Plex genre in the author's setup)
	Rating   string   `json:"rating,omitempty"`  // second one
	Handle   string   `json:"handle"`

	Folder        bool    `json:"folder"`      // an archive exists for it
	Kept          int     `json:"kept"`        // entries in the archive = videos ytdl-sub still tracks
	LastUpload    string  `json:"last_upload"` // YYYY-MM-DD of the newest archive entry
	Files         int     `json:"files"`       // video files under the archive's folder
	Files30       int     `json:"files30"`     // of them, downloaded in the last 30 days
	Files90       int     `json:"files90"`     // and 90
	GB            float64 `json:"gb"`
	GB30          float64 `json:"gb30"`
	LastFile      string  `json:"last_file"` // YYYY-MM-DD of the newest video file
	DaysSinceFile *int    `json:"days_since_file"`

	Check        string `json:"check"`      // "ok", "failed" or "" when the last summary did not list it
	CheckedAt    string `json:"checked_at"` // end of the run that produced the verdict
	CheckAdded   *int   `json:"check_added"`
	CheckRemoved *int   `json:"check_removed"`
	Status       string `json:"status"` // active, quiet, silent, never, failed, unchecked
}

// Run is one cron slot, from the wrapper's "Cron started" line to its end line.
type Run struct {
	Instance string   `json:"instance"`
	Sub      string   `json:"sub,omitempty"` // a check of one subscription: its name
	Start    string   `json:"start"`
	End      string   `json:"end"`
	Minutes  *float64 `json:"minutes"`
	Result   string   `json:"result"` // ok, error, lock, timeout, killed, running
	Added    *int     `json:"added"`
	Removed  *int     `json:"removed"`
	Kept     *int     `json:"kept"`
	Summary  string   `json:"summary,omitempty"` // the Total line's verdict word (Success, Error...)
	Source   string   `json:"source,omitempty"`  // where the counts came from: summary, archive, none
}

// Instance is the card: one ytdl-sub instance and the agent beside it.
type Instance struct {
	Instance string `json:"instance"`
	Host     string `json:"host"`
	Mode     string `json:"mode"` // vpn or direct, from the exit provider
	Status   string `json:"status"`
	Health   string `json:"health"` // healthy, unhealthy, starting, unknown
	Since    string `json:"since"`
	Exit     string `json:"exit"`    // "France, Paris", "own address", "not connected"
	ExitIP   string `json:"exit_ip"` // never shown by the page by default

	Channels int     `json:"channels"`
	Keep     int     `json:"keep"`
	GB       float64 `json:"gb"`
	GB30     float64 `json:"gb30"`
	Videos30 int     `json:"videos30"`
	Silent   int     `json:"silent"` // channels never, silent or failed

	LastStart   string   `json:"last_start"`
	LastEnd     string   `json:"last_end"`
	LastMinutes *float64 `json:"last_minutes"`
	LastResult  string   `json:"last_result"`
	LastAdded   *int     `json:"last_added"`
	LastRemoved *int     `json:"last_removed"`
	Runs24h     int      `json:"runs_24h"`
	Failed24h   int      `json:"failed_24h"`
	AvgMinutes  *float64 `json:"avg_minutes"`

	Agent        string   `json:"agent,omitempty"`        // agent version
	YtdlSub      string   `json:"ytdl_sub,omitempty"`     // ytdl-sub version when known
	Capabilities []string `json:"capabilities,omitempty"` // run, stop, reconnect: what POSTs the agent accepts
}

// DailyRow counts, for one day, the kept videos per instance by their upload date.
type DailyRow map[string]any // {"day": "2026-10-26", "<instance>": n, ...}

// Meta describes a collection: when, how long, and what went wrong.
type Meta struct {
	Generated string   `json:"generated"`
	Seconds   float64  `json:"seconds"`
	Warnings  []string `json:"warnings"`
	Agents    []string `json:"agents,omitempty"` // dashboard: the agents it merged
}

// Summary is the fleet in one object, for a home dashboard's single card: the dashboard serves it as
// /data/summary.json next to the five files the page uses.
type Summary struct {
	Generated   string  `json:"generated"`
	Instances   int     `json:"instances"`
	Healthy     int     `json:"healthy"`
	Unreachable int     `json:"unreachable"`
	Running     int     `json:"running"` // runs in progress right now
	Channels    int     `json:"channels"`
	Silent      int     `json:"silent"` // channels never, silent or failed
	Videos30    int     `json:"videos30"`
	GB          float64 `json:"gb"`
	Runs24h     int     `json:"runs_24h"`
	Failed24h   int     `json:"failed_24h"`
	LastEnd     string  `json:"last_end"`    // the most recent run end across the fleet
	LastResult  string  `json:"last_result"` // that instance's last result
	Warnings    int     `json:"warnings"`
}

// Status is the agent's own answer on GET /api/status.
type Status struct {
	Instance Instance `json:"instance"`
	Running  bool     `json:"running"`
	Meta     Meta     `json:"meta"`
}
