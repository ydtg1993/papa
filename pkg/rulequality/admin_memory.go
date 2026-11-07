package rulequality

import (
	"context"
	"sort"
)

func (s *MemoryStore) Requests(ctx context.Context, before, reportID string) (RequestPage, error) {
	out := RequestPage{Items: []Request{}}
	if ctx == nil || before != "" && !validCode(before, 128) || reportID != "" && !validCode(reportID, 128) {
		return out, ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return out, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.requests {
		if (before == "" || r.ID < before) && (reportID == "" || r.ReportID == reportID) {
			out.Items = append(out.Items, r)
		}
	}
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].ID > out.Items[j].ID })
	if len(out.Items) > 20 {
		out.Items = out.Items[:20]
		out.Next = out.Items[19].ID
	}
	out.Scanned = len(out.Items)
	return out, nil
}
func (s *MemoryStore) Freeze(ctx context.Context, in FreezeInput, operator string) (Report, error) {
	return FreezeReport(ctx, s, in, operator)
}
func (s *MemoryStore) ReportOperation(ctx context.Context, id string) (Report, error) {
	return ReportByOperation(ctx, s, id)
}
