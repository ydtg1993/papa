package browser

type instanceState uint8

const (
	instanceIdle instanceState = iota
	instanceReserved
	instanceBorrowed
)

// InstanceStats counts managed browser instances, not Chromium child processes.
// Reserved means an existing instance selected from this pool is waiting for a
// resource permit. A not-yet-created physical slot is never included in Live.
type InstanceStats struct {
	Capacity int `json:"capacity"`
	Live     int `json:"live"`
	Borrowed int `json:"borrowed"`
	Reserved int `json:"reserved"`
	Idle     int `json:"idle"`
}
type PoolStats struct {
	InstanceStats
	Closed bool          `json:"closed"`
	Proxy  InstanceStats `json:"proxy"`
	Direct InstanceStats `json:"direct"`
}

func (p *Pool) registerInstance(b *Browser) {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	if b.closed.Load() {
		return
	}
	if p.instances == nil {
		p.instances = make(map[*Browser]instanceState)
	}
	p.instances[b] = instanceBorrowed
}
func (p *Pool) unregisterInstance(b *Browser) {
	p.statsMu.Lock()
	delete(p.instances, b)
	p.statsMu.Unlock()
}
func (p *Pool) setInstanceState(b *Browser, state instanceState) {
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	if _, ok := p.instances[b]; ok && !b.closed.Load() {
		p.instances[b] = state
	}
}

// Stats is a read-only lifecycle snapshot. Closing the pool retires idle
// instances; borrowed instances remain live until returned/closed, and are
// reported as such. It does not probe CDP or wake/create browsers.
func (p *Pool) Stats() PoolStats {
	if p == nil {
		return PoolStats{}
	}
	p.statsMu.Lock()
	defer p.statsMu.Unlock()
	out := PoolStats{Closed: p.closed.Load(), Proxy: InstanceStats{Capacity: cap(p.browsers)}, Direct: InstanceStats{Capacity: cap(p.direct)}}
	for b, state := range p.instances {
		class := &out.Proxy
		if !b.useProxy {
			class = &out.Direct
		}
		class.Live++
		switch state {
		case instanceIdle:
			class.Idle++
		case instanceReserved:
			class.Reserved++
		case instanceBorrowed:
			class.Borrowed++
		}
	}
	out.InstanceStats = InstanceStats{Capacity: out.Proxy.Capacity + out.Direct.Capacity, Live: out.Proxy.Live + out.Direct.Live, Borrowed: out.Proxy.Borrowed + out.Direct.Borrowed, Reserved: out.Proxy.Reserved + out.Direct.Reserved, Idle: out.Proxy.Idle + out.Direct.Idle}
	return out
}
