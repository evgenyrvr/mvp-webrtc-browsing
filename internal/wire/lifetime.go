package wire

import "time"

// MaxSessionLifetime is a hard pilot limit, independent of activity or reconnect.
const MaxSessionLifetime = 30 * time.Minute
