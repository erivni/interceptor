// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package gcc

import (
	"testing"
	"time"

	"github.com/pion/interceptor/internal/cc"
	"github.com/stretchr/testify/assert"
)

func TestRateCalculator(t *testing.T) {
	//t0 := time.Now()
	cases := []struct {
		name     string
		acks     []cc.Acknowledgment
		expected []int
	}{
		// {
		// 	name:     "emptyCreatesNoRate",
		// 	acks:     []cc.Acknowledgment{},
		// 	expected: []int{},
		// },
		// {
		// 	name: "ignoresZeroArrivalTimes",
		// 	acks: []cc.Acknowledgment{{
		// 		SequenceNumber: 0,
		// 		Size:           0,
		// 		Departure:      time.Time{},
		// 		Arrival:        time.Time{},
		// 	}},
		// 	expected: []int{},
		// },
		// {
		// 	name: "singleAckCreatesRate",
		// 	acks: []cc.Acknowledgment{{
		// 		SequenceNumber: 0,
		// 		Size:           1000,
		// 		Departure:      time.Time{},
		// 		Arrival:        t0,
		// 	}},
		// 	expected: []int{8000},
		// },
		// {
		// 	name: "twoAcksCalculateCorrectRates",
		// 	acks: []cc.Acknowledgment{{
		// 		SequenceNumber: 0,
		// 		Size:           125,
		// 		Departure:      time.Time{},
		// 		Arrival:        t0,
		// 	}, {
		// 		SequenceNumber: 0,
		// 		Size:           125,
		// 		Departure:      time.Time{},
		// 		Arrival:        t0.Add(100 * time.Millisecond),
		// 	}},
		// 	expected: []int{1000, 20_000},
		// },
		{
			name: "steadyACKsCalculateCorrectRates",
			acks: getACKStream(10, 1200, 100*time.Millisecond),
			expected: []int{
				9_600,
				213333,
				160000,
				142222,
				133333,
				128000,
				130909,
				133953,
				137142,
				140487,
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			rc := newRateCalculator(500 * time.Millisecond)
			in := make(chan []cc.Acknowledgment)
			out := make(chan int)
			onRateUpdate := func(rate int) {
				out <- rate
			}
			go func() {
				defer close(out)
				rc.run(in, onRateUpdate)
			}()
			go func() {
				in <- tc.acks
				close(in)
			}()

			received := []int{}
			for r := range out {
				received = append(received, r)
			}
			assert.Equal(t, tc.expected, received)
		})
	}
}

func getACKStream(length int, size int, interval time.Duration) []cc.Acknowledgment {
	res := []cc.Acknowledgment{}
	t0 := time.Now()
	t1 := t0
	for i := 0; i < length; i++ {
		t1 = t1.Add(interval/10)
		res = append(res, cc.Acknowledgment{
			Size:    size,
			Departure: t1,
			Arrival: t0,
		})
		t0 = t0.Add(interval)
	}
	return res
}
