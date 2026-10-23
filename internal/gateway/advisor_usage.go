package gateway

import (
	"encoding/json"
	"strings"

	"github.com/yetone/magpie/internal/usage"
)

// advisorRecords reads only the additional server-side model calls. Message
// iterations are already in the executor totals and must not be counted twice.
func advisorRecords(parent usage.Record, u Usage) []usage.Record {
	if u.Iterations == nil {
		return nil
	}
	var records []usage.Record
	for _, raw := range *u.Iterations {
		var iteration struct {
			aUsage
			Type  string `json:"type"`
			Model string `json:"model"`
		}
		if json.Unmarshal(raw, &iteration) != nil || iteration.Type != "advisor_message" {
			continue
		}
		model := strings.TrimSpace(iteration.Model)
		v := iteration.usage()
		if model == "" || v.Input < 0 || v.Output < 0 || v.CacheRead < 0 || v.CacheWrite < 0 || v.CacheWrite1h < 0 ||
			v.Input+v.Output+v.CacheRead+v.CacheWrite == 0 {
			continue
		}
		// Identity and billing account belong to the enclosing call. Its
		// response id, status, timing and bodies do not describe the advisor.
		records = append(records, usage.Record{
			Operation: usage.AdvisorOperation, Kind: "advisor", RouteID: parent.RouteID,
			Time: parent.Time, Agent: parent.Agent, Via: parent.Via,
			Provider: parent.Provider, Host: parent.Host,
			ProviderKeyID: parent.ProviderKeyID, ProviderKeyName: parent.ProviderKeyName,
			ProviderAccount: parent.ProviderAccount,
			CallerKeyID:     parent.CallerKeyID, CallerKeyName: parent.CallerKeyName,
			Session: parent.Session, NativeSession: parent.NativeSession,
			Subagent: parent.Subagent, ParentAgent: parent.ParentAgent,
			Model: model, Served: model, Input: v.Input, Output: v.Output,
			CacheRead: v.CacheRead, CacheWrite: v.CacheWrite, CacheWrite1h: v.CacheWrite1h,
		})
	}
	return records
}
