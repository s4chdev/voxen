package audio

import "sync/atomic"

// liveContexts counts miniaudio contexts that have been created but not freed.
//
// It exists for tests. A context leaked per failed daemon construction eventually
// exhausts PulseAudio's client slots, which presents as an abort inside cgo in an
// unrelated test rather than as an error at the site of the leak.
var liveContexts atomic.Int64

// LiveCount reports how many backend contexts are currently allocated.
func LiveCount() int64 { return liveContexts.Load() }
