package browser

type instanceState uint8

const (
	instanceIdle instanceState = iota
	instanceReserved
	instanceBorrowed
	instanceReturning
	instanceClosing
)

// InstanceStats counts managed browser instances, not Chromium child processes.
// Reserved means an existing instance selected from this pool is waiting for a
// resource permit. A not-yet-created physical slot is never included in Live.
type InstanceStats struct {
	Capacity   int  `json:"capacity"`
	Target     int  `json:"target"`
	Creating   int  `json:"creating"`
	Closing    int  `json:"closing"`
	Waiting    int  `json:"waiting"`
	Converging bool `json:"converging"`
	Live       int  `json:"live"`
	Borrowed   int  `json:"borrowed"`
	Reserved   int  `json:"reserved"`
	Idle       int  `json:"idle"`
}
type PoolStats struct {
	InstanceStats
	Closed bool          `json:"closed"`
	Proxy  InstanceStats `json:"proxy"`
	Direct InstanceStats `json:"direct"`
}

func (p *Pool) markClosing(b *Browser) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.instances[b]; !ok {
		return
	}
	p.instances[b] = instanceClosing
	idle, _, _, _, _ := p.classLocked(b.useProxy)
	for i, v := range *idle {
		if v == b {
			*idle = append((*idle)[:i], (*idle)[i+1:]...)
			break
		}
	}
}
func (p *Pool) unregisterInstance(b *Browser) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, ok := p.instances[b]; !ok {
		return
	}
	delete(p.instances, b)
	_, alive, _, _, _ := p.classLocked(b.useProxy)
	alive.Add(-1)
	p.cond.Broadcast()
}

// Stats is a read-only lifecycle snapshot. Closing the pool retires idle
// instances; borrowed instances remain live until returned/closed, and are
// reported as such. It does not probe CDP or wake/create browsers.
func (p *Pool) Stats() PoolStats {
	if p == nil {
		return PoolStats{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.initLocked()
	out := PoolStats{Closed: p.closed.Load(), Proxy: InstanceStats{Capacity: p.proxyTarget, Target: p.proxyTarget, Creating: p.proxyCreating, Waiting: p.proxyWaiting}, Direct: InstanceStats{Capacity: p.directTarget, Target: p.directTarget, Creating: p.directCreating, Waiting: p.directWaiting}}
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
		case instanceClosing:
			class.Closing++
		case instanceBorrowed, instanceReturning:
			class.Borrowed++
		}
	}
	out.Proxy.Converging = out.Proxy.Live+out.Proxy.Creating > out.Proxy.Target || out.Proxy.Closing > 0
	out.Direct.Converging = out.Direct.Live+out.Direct.Creating > out.Direct.Target || out.Direct.Closing > 0
	out.InstanceStats = InstanceStats{Capacity: out.Proxy.Capacity + out.Direct.Capacity, Live: out.Proxy.Live + out.Direct.Live, Borrowed: out.Proxy.Borrowed + out.Direct.Borrowed, Reserved: out.Proxy.Reserved + out.Direct.Reserved, Idle: out.Proxy.Idle + out.Direct.Idle}
	out.Target = out.Capacity
	out.Creating = out.Proxy.Creating + out.Direct.Creating
	out.Closing = out.Proxy.Closing + out.Direct.Closing
	out.Waiting = out.Proxy.Waiting + out.Direct.Waiting
	out.Converging = out.Proxy.Converging || out.Direct.Converging
	return out
}
