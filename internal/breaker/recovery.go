package breaker

import "time"

const MaxRecoveryStages = 128

// Recovery is control data. Display truncation and redaction must never change
// its identities, coverage or admission state. All access is owned by Engine's lock.
type Recovery struct {
	Version                                                             int // recovery format; zero predates serial sample validation
	ID, Site, Stage                                                     string
	State                                                               string
	Revision, Epoch                                                     uint64
	StartedAt, UpdatedAt, LastFailureAt, NextProbeAt, Deadline, FinalAt time.Time
	RetryAfterUntil                                                     time.Time
	CountersResetAt                                                     time.Time
	ProbeAttempts, Successes                                            int
	Checks                                                              []StageCheck
	ChecksLimited                                                       bool
	MixedSamples                                                        bool
	RecheckAt                                                           time.Time
	Cycle                                                               int
	HistorySequence                                                     uint64
	StoppedAt                                                           time.Time
	SampleID, SampleStage                                               string
	SampleDeadline                                                      time.Time // selection time + 10 minutes, capped by the cycle budget
	PendingFailure                                                      *PendingFailure
}

// A serial sample's business result awaiting terminal acknowledgement. Keep
// the original round and classification; recovery retries SQL, never business.
type PendingFailure struct {
	TaskID                                            int
	Stage, URL, Error, Category, Fingerprint          string
	Repeat, Reprocess                                 int
	At, SampleDeadline                                time.Time
	FailureKind, CompensationMode, CompensationReason string
	NotBefore                                         time.Time
	LastAttempt                                       time.Time
	Attempts                                          int
}

type RecoverySample struct {
	TaskID    int64
	URL       string
	Successes int
}

type StageCheck struct {
	Stage, State                  string
	Round                         int
	StartedAt, FinishedAt         time.Time
	Detail                        string
	TaskSuccesses                 int
	Verification, Coverage        string
	ProbeSuccesses                int
	RequiredTasks, RequiredProbes int
	Samples                       []RecoverySample
	SuccessIDs                    []string
	TimedOutIDs                   []string // current cycle only; never healthy even if other samples pass
}

func (r Recovery) Active() bool { return r.State != "closed" && r.State != "recovered" }

// Checkpoint preserves exact failure round identities and their original times.
// Successful task proofs deliberately do not survive process restart.
type Checkpoint struct {
	Failures       []Outcome
	OverflowUntil  time.Time
	Anonymous      uint64
	LegacyFailures int
	LegacyUntil    time.Time
}

func (d *Detector) Checkpoint() Checkpoint {
	d.mu.Lock()
	defer d.mu.Unlock()
	c := Checkpoint{Failures: append([]Outcome(nil), d.failures...), OverflowUntil: d.overflowUntil, Anonymous: d.anonymous, LegacyFailures: d.legacyFailures, LegacyUntil: d.legacyUntil}
	for i := range c.Failures {
		c.Failures[i].Error, c.Failures[i].Kind = "", ""
	}
	return c
}

func (d *Detector) Restore(c Checkpoint, epoch uint64, now time.Time) {
	d.ResetEpoch(epoch)
	d.mu.Lock()
	defer d.mu.Unlock()
	d.anonymous = c.Anonymous
	d.overflowUntil = c.OverflowUntil
	d.legacyFailures, d.legacyUntil = c.LegacyFailures, c.LegacyUntil
	for _, o := range c.Failures {
		o.Epoch = epoch
		d.failures = append(d.failures, o)
	}
	d.last = now
	d.prune(now)
	if len(d.failures) > d.policy.Capacity {
		// A smaller configuration cannot silently discard still-valid failures.
		for _, o := range d.failures {
			until := o.At.Add(max(d.cfg.Window, d.policy.ShortWindow))
			if until.After(d.overflowUntil) {
				d.overflowUntil = until
			}
		}
	}
}
