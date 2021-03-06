package node

import (
	"sync/atomic"
	"testing"
	"time"

	ps "github.com/prometheus/client_golang/prometheus"
	io_prometheus_clients "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/youzan/ZanRedisDB/metric"
)

var slowRefuseCost = time.Millisecond * time.Duration(SlowRefuseCostMs)

func TestSlowLimiter_CanPass(t *testing.T) {
	type fields struct {
		slowCounter  int64
		limiterOn    int32
		slowHistorys [maxSlowLevel]map[string]int64
		lastSlowTs   int64
	}
	type args struct {
		cmd    string
		prefix string
	}
	var emptySlows [maxSlowLevel]map[string]int64
	slow100s := make(map[string]int64)
	slow50s := make(map[string]int64)
	slow10s := make(map[string]int64)
	emptySlows[0] = slow10s
	emptySlows[1] = slow50s
	emptySlows[2] = slow100s
	var allSlows [maxSlowLevel]map[string]int64
	slow100sTestTable := make(map[string]int64)
	slow100sTestTable["set test_table"] = 10
	slow50sTestTable := make(map[string]int64)
	slow50sTestTable["set test_table"] = 20
	slow10sTestTable := make(map[string]int64)
	slow10sTestTable["set test_table"] = 30
	allSlows[0] = slow10sTestTable
	allSlows[1] = slow50sTestTable
	allSlows[2] = slow100sTestTable
	var slow50s_10sHist [maxSlowLevel]map[string]int64
	slow50s_10sHist[0] = slow10sTestTable
	slow50s_10sHist[1] = slow50sTestTable
	var slow100sHist [maxSlowLevel]map[string]int64
	slow100sHist[2] = slow100sTestTable
	var slow50sHist [maxSlowLevel]map[string]int64
	slow50sHist[1] = slow50sTestTable
	var slow10sHist [maxSlowLevel]map[string]int64
	slow10sHist[0] = slow10sTestTable
	tn := time.Now()
	tests := []struct {
		name   string
		fields fields
		args   args
		want   bool
	}{
		// real no slow
		{"canpass_noslow1", fields{0, 1, emptySlows, 0}, args{"set", "test_table"}, true},
		// no recorded table
		{"canpass_noslow_record", fields{maxSlowThreshold, 1, emptySlows, tn.UnixNano()}, args{"set", "test_table"}, true},
		// last slow is long ago
		{"canpass_slow_last_long_ago", fields{maxSlowThreshold, 1, allSlows, tn.Add(-1 * time.Hour).UnixNano()}, args{"set", "test_table"}, true},
		// mid slow should only refuce 100ms write
		{"canpass_below100ms_in_small_slow", fields{smallSlowThreshold, 1, slow50s_10sHist, tn.UnixNano()}, args{"set", "test_table"}, true},
		{"cannotpass_100ms_in_small_slow", fields{smallSlowThreshold, 1, slow100sHist, tn.UnixNano()}, args{"set", "test_table"}, false},
		{"canpass_below50ms_in_mid_slow", fields{midSlowThreshold, 1, slow10sHist, tn.UnixNano()}, args{"set", "test_table"}, true},
		{"cannotpass_50ms_in_mid_slow", fields{midSlowThreshold, 1, slow50sHist, tn.UnixNano()}, args{"set", "test_table"}, false},
		{"cannotpass_100ms_in_mid_slow", fields{midSlowThreshold, 1, slow100sHist, tn.UnixNano()}, args{"set", "test_table"}, false},
		{"canpass_below10ms_in_heavy_slow", fields{heavySlowThreshold, 1, emptySlows, tn.UnixNano()}, args{"set", "test_table"}, true},
		{"cannotpass_10ms_in_heavy_slow", fields{heavySlowThreshold, 1, slow10sHist, tn.UnixNano()}, args{"set", "test_table"}, false},
		{"cannotpass_50ms_in_heavy_slow", fields{heavySlowThreshold, 1, slow50sHist, tn.UnixNano()}, args{"set", "test_table"}, false},
		{"cannotpass_100ms_in_heavy_slow", fields{heavySlowThreshold, 1, slow100sHist, tn.UnixNano()}, args{"set", "test_table"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sl := &SlowLimiter{
				slowCounter:  tt.fields.slowCounter,
				limiterOn:    tt.fields.limiterOn,
				slowHistorys: tt.fields.slowHistorys,
				lastSlowTs:   tt.fields.lastSlowTs,
			}
			if got := sl.CanPass(tn.UnixNano(), tt.args.cmd, tt.args.prefix); got != tt.want {
				t.Errorf("SlowLimiter.CanPass() = %v, want %v", got, tt.want)
			}
		})
	}
	counter := metric.SlowLimiterRefusedCnt.With(ps.Labels{
		"table": "test_table",
		"cmd":   "set",
	})
	out := io_prometheus_clients.Metric{}
	counter.Write(&out)
	assert.Equal(t, float64(6), *out.Counter.Value)
}

func TestSlowLimiter_SlowToNoSlow(t *testing.T) {
	enableSlowLimiterTest = true
	defer func() {
		enableSlowLimiterTest = false
	}()
	sl := NewSlowLimiter("test")
	sl.Start()
	defer sl.Stop()
	cnt := 0
	atomic.StoreInt64(&sl.slowCounter, midSlowThreshold)
	oldTs := time.Now().UnixNano()
	atomic.StoreInt64(&sl.lastSlowTs, oldTs)
	sl.RecordSlowCmd("test", "test_table", slowRefuseCost)
	sl.RecordSlowCmd("test", "test_table", slowRefuseCost)
	sl.RecordSlowCmd("test", "test_table", slowRefuseCost)
	assert.True(t, !sl.CanPass(time.Now().UnixNano(), "test", "test_table"))
	// use old ts to check pass to make sure we are passed by the cleared slow record
	for {
		cnt++
		if sl.CanPass(time.Now().UnixNano(), "test", "test_table") && sl.CanPass(oldTs, "test", "test_table") {
			break
		}
		// should sleep more than ticker
		// in test the slow down ticker is more faster
		time.Sleep(time.Second)
	}
	t.Logf("slow to noslow cnt : %v", cnt)
	// in test the slow down ticker is more faster
	assert.True(t, cnt >= smallSlowThreshold/4)
	assert.True(t, cnt < heavySlowThreshold)
}

func TestSlowLimiter_NoSlowToSlow(t *testing.T) {
	sl := NewSlowLimiter("test")
	sl.Start()
	defer sl.Stop()
	cnt := 0
	for {
		sl.RecordSlowCmd("test", "test_table", slowRefuseCost)
		sl.MaybeAddSlow(time.Now().UnixNano(), slowRefuseCost, "test", "test_table")
		cnt++
		if !sl.CanPass(time.Now().UnixNano(), "test", "test_table") {
			break
		}
	}
	t.Logf("noslow to slow cnt : %v", cnt)
	assert.True(t, cnt >= smallSlowThreshold)
	assert.True(t, cnt < heavySlowThreshold)
}
