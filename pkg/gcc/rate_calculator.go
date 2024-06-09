// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package gcc

import (
	"time"

	"github.com/pion/interceptor/internal/cc"
)

type rateCalculator struct {
	window time.Duration
}

func newRateCalculator(window time.Duration) *rateCalculator {
	return &rateCalculator{
		window: window,
	}
}

func (c *rateCalculator) run(in <-chan []cc.Acknowledgment, onRateUpdate func(int)) {
	var history []cc.Acknowledgment
	var historyDeparture []cc.Acknowledgment
	init := false
	sum := 0
	for acks := range in {
		for _, next := range acks {
			historyDeparture = append(historyDeparture, next)
			
			if next.Arrival.IsZero() {
				// Ignore packet if it didn't arrive
				continue
			}
			history = append(history, next)
			sum += next.Size

			if !init {
				init = true
				// Don't know any timeframe here, only arrival of last packet,
				// which is by definition in the window that ends with the last
				// arrival time
				onRateUpdate(next.Size * 8)
				continue
			}

			del := 0
			for _, ack := range historyDeparture {
				deadline := next.Departure.Add(-c.window)
				if !ack.Departure.Before(deadline) {
					break
				}
				del++
			}
			historyDeparture = historyDeparture[del:]

			del = 0
			for _, ack := range history {
				deadline := next.Arrival.Add(-c.window)
				if !ack.Arrival.Before(deadline) {
					break
				}
				del++
				sum -= ack.Size
			}
			history = history[del:]
			if len(history) == 0 || len(historyDeparture) == 0 {
				onRateUpdate(0)
				continue
			}

			// Calculate total departure delta for the remaining history
			totalDepartureDelta := time.Duration(0)
			for i := 1; i < len(historyDeparture); i++ {
				totalDepartureDelta += historyDeparture[i].Departure.Sub(historyDeparture[i-1].Departure)
			}

			dt := next.Arrival.Sub(history[0].Arrival) - totalDepartureDelta
			bits := 8 * sum
			rate := int(float64(bits) / dt.Seconds())
			onRateUpdate(rate)
		}
	}
}
