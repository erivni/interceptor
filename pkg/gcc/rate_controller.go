// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package gcc

import (
	"fmt"
	"sync"
	"time"
)

// const (
// 	decreaseEMAAlpha = 0.95
// 	beta             = 0.85
// )

type RateControllerOptions struct {
	IncreaseTimeThreshold time.Duration
	IncreaseFactor        float64
	DecreaseTimeThreshold time.Duration
	DecreaseFactor        float64
}

type rateController struct {
	now                  now
	initialTargetBitrate int
	minBitrate           int
	maxBitrate           int

	dsWriter func(DelayStats)

	lock               sync.Mutex
	init               bool
	delayStats         DelayStats
	target             int
	lastUpdate         time.Time
	lastState          state
	latestRTT          time.Duration
	latestReceivedRate int
	//latestDecreaseRate *exponentialMovingAverage

	lastIncrease          time.Time
	lastDecrease          time.Time
	rateControllerOptions *RateControllerOptions

	bitrateControlBucketsManager *Manager
}

// type exponentialMovingAverage struct {
// 	average      float64
// 	variance     float64
// 	stdDeviation float64
// }

// func (a *exponentialMovingAverage) update(value float64) {
// 	if a.average == 0.0 {
// 		a.average = value
// 	} else {
// 		x := value - a.average
// 		a.average += decreaseEMAAlpha * x
// 		a.variance = (1 - decreaseEMAAlpha) * (a.variance + decreaseEMAAlpha*x*x)
// 		a.stdDeviation = math.Sqrt(a.variance)
// 	}
// }

func newRateController(now now, initialTargetBitrate, minBitrate, maxBitrate int, rateControllerOptions *RateControllerOptions, dsw func(DelayStats)) *rateController {
	if rateControllerOptions == nil {
		defaultOptions := RateControllerOptions{
			IncreaseTimeThreshold: 100 * time.Millisecond,
			IncreaseFactor:        1.15,
			DecreaseTimeThreshold: 100 * time.Millisecond,
			DecreaseFactor:        0.85,
		}
		rateControllerOptions = &defaultOptions
	}

	bitrateControlBucketsManager := BitrateControlBucketsConfig{
		BitrateStableThreshold:              15*25,
		HandleUnstableBitrateGracePeriodSec: 2,
		BitrateBucketIncrement:              250000,
		BackoffDurationsSec:                 []float64{0, 15, 30, 60},
	}

	manager  := NewManager(&bitrateControlBucketsManager)
	manager.InitializeBuckets(uint64(maxBitrate))

	return &rateController{
		now:                          now,
		initialTargetBitrate:         initialTargetBitrate,
		minBitrate:                   minBitrate,
		maxBitrate:                   maxBitrate,
		dsWriter:                     dsw,
		init:                         false,
		delayStats:                   DelayStats{},
		target:                       initialTargetBitrate,
		lastUpdate:                   time.Time{},
		lastState:                    stateIncrease,
		latestRTT:                    0,
		latestReceivedRate:           0,
		bitrateControlBucketsManager: manager,
		//latestDecreaseRate:   &exponentialMovingAverage{},

		rateControllerOptions: rateControllerOptions,
		lastIncrease:          time.Time{},
		lastDecrease:          time.Time{},
	}
}

func (c *rateController) onReceivedRate(rate int) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.latestReceivedRate = rate
}

func (c *rateController) updateRTT(rtt time.Duration) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.latestRTT = rtt
}

func (c *rateController) onDelayStats(ds DelayStats) {
	now := time.Now()

	if !c.init {
		c.delayStats = ds
		c.delayStats.State = stateIncrease
		c.init = true
		return
	}
	c.delayStats = ds
	c.delayStats.State = c.delayStats.State.transition(ds.Usage)

	if c.delayStats.State == stateHold {
		c.bitrateControlBucketsManager.HandleBitrateNormal(uint64(c.target))
		return
	}

	var next DelayStats

	c.lock.Lock()

	switch c.delayStats.State {
	case stateHold:
		// should never occur due to check above, but makes the linter happy
	case stateIncrease:
		c.bitrateControlBucketsManager.HandleBitrateNormal(uint64(c.target))

		suggestedTarget := clampInt(c.increase(now), c.minBitrate, c.maxBitrate)
		err := c.bitrateControlBucketsManager.CanIncreaseToBitrate(uint64(c.target), uint64(suggestedTarget))
		if err == nil {
			c.target = suggestedTarget
		}

		next = DelayStats{
			Measurement:      c.delayStats.Measurement,
			Estimate:         c.delayStats.Estimate,
			Threshold:        c.delayStats.Threshold,
			LastReceiveDelta: c.delayStats.LastReceiveDelta,
			Usage:            c.delayStats.Usage,
			State:            c.delayStats.State,
			TargetBitrate:    c.target,
			ReceivedBitrate:  c.latestReceivedRate,
		}

	case stateDecrease:
		c.bitrateControlBucketsManager.HandleBitrateDecrease(uint64(c.target))
		c.target = clampInt(c.decrease(now), c.minBitrate, c.maxBitrate)

		next = DelayStats{
			Measurement:      c.delayStats.Measurement,
			Estimate:         c.delayStats.Estimate,
			Threshold:        c.delayStats.Threshold,
			LastReceiveDelta: c.delayStats.LastReceiveDelta,
			Usage:            c.delayStats.Usage,
			State:            c.delayStats.State,
			TargetBitrate:    c.target,
			ReceivedBitrate:  c.latestReceivedRate,
		}
	}

	c.lock.Unlock()

	c.dsWriter(next)
}

func (c *rateController) increase(now time.Time) int {
	// if c.latestDecreaseRate.average > 0 && float64(c.latestReceivedRate) > c.latestDecreaseRate.average-3*c.latestDecreaseRate.stdDeviation &&
	// 	float64(c.latestReceivedRate) < c.latestDecreaseRate.average+3*c.latestDecreaseRate.stdDeviation {
	// 	bitsPerFrame := float64(c.target) / 25.0
	// 	packetsPerFrame := math.Ceil(bitsPerFrame / (1200 * 8))
	// 	expectedPacketSizeBits := bitsPerFrame / packetsPerFrame

	// 	responseTime := 100*time.Millisecond + c.latestRTT
	// 	alpha := 0.5 * math.Min(float64(now.Sub(c.lastUpdate).Milliseconds())/float64(responseTime.Milliseconds()), 1.0)
	// 	increase := int(math.Max(1000.0, alpha*expectedPacketSizeBits))
	// 	c.lastUpdate = now
	// 	return int(math.Min(float64(c.target+increase), 1.5*float64(c.latestReceivedRate)))
	// }
	// eta := math.Pow(1.08, math.Min(float64(now.Sub(c.lastUpdate).Milliseconds())/1000, 1.0))
	// c.lastUpdate = now

	// rate := int(eta * float64(c.target))

	// // maximum increase to 1.5 * received rate
	// received := int(1.5 * float64(c.latestReceivedRate))
	// if rate > received && received > c.target {
	// 	return received
	// }

	// if rate < c.target {
	// 	return c.target
	// }

	// factor := math.Min(math.Pow(1.5, now.Sub(c.lastUpdate).Seconds()), 1.15)
	// rate := int(float64(c.target) * factor)
	// c.lastUpdate = now
	// rate := c.target
	// if time.Since(c.lastIncrease) > c.rateControllerOptions.IncreaseTimeThreshold {
	// 	c.lastIncrease = time.Now()
	// 	rate = clampInt(int(c.rateControllerOptions.IncreaseFactor*float64(c.target)), c.minBitrate, c.maxBitrate)
	// }
	newtarget, _ := c.nextHigherBitrate(uint64(c.target))
	return int(newtarget)
}

func (c *rateController) decrease(now time.Time) int {
	// target := int(beta * float64(c.latestReceivedRate))
	// c.latestDecreaseRate.update(float64(c.latestReceivedRate))
	// c.lastUpdate = c.now()

	// factor := math.Max(math.Pow(0.5, now.Sub(c.lastUpdate).Seconds()), 0.85)
	// target := int(float64(c.target) * factor)
	// c.lastUpdate = now
	// return target

	// rate := c.target
	// if time.Since(c.lastDecrease) > c.rateControllerOptions.DecreaseTimeThreshold {
	// 	c.lastIncrease = time.Now()
	// 	rate = clampInt(int(c.rateControllerOptions.DecreaseFactor*float64(c.target)), c.minBitrate, c.maxBitrate)
	// }

	newtarget, _ := c.nextLowerBitrate(uint64(c.target))
	return int(newtarget)
}

func (c *rateController) nextHigherBitrate(currentBitrate uint64) (uint64, error) {
	desiredBitrate := currentBitrate + 250000
	if desiredBitrate > uint64(c.maxBitrate) ||
		desiredBitrate < currentBitrate { // cover uint64-wrap cases where desiredBitrate is higher than uint64-max
		desiredBitrate = uint64(c.maxBitrate)
	}

	if currentBitrate < desiredBitrate {
		return desiredBitrate, nil
	}
	return 0, fmt.Errorf("current bitrate is already at max")
}

func (c *rateController) nextLowerBitrate(currentBitrate uint64) (uint64, error) {
	desiredBitrate := currentBitrate - 250000
	if desiredBitrate < uint64(c.minBitrate) ||
		desiredBitrate > currentBitrate { // cover uint64-wrap cases where desiredBitrate is lowered under zero
		desiredBitrate = uint64(c.minBitrate)
	}
	if currentBitrate > desiredBitrate {
		return desiredBitrate, nil
	}

	return 0, fmt.Errorf("current bitrate is already at min")
}
