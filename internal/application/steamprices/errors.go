package steamprices

import "errors"

// ErrNotTracked reports a price request for an item the system has never
// recorded (no stored price row from sync). Callers map it to 404
// ITEM_NOT_TRACKED without burning a Steam provider call.
var ErrNotTracked = errors.New("steamprices: item not tracked")

// ErrUpstream reports a transient Steam failure (throttle, 5xx, network)
// behind a price request. Callers map it to 502, never to 404: a dead
// endpoint is not a missing price.
var ErrUpstream = errors.New("steamprices: steam upstream unavailable")
