// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package gcc

import (
	"sync"
	"time"
)

type RateControllerBucketsOptions struct {
	IncreaseTimeThreshold time.Duration
	DecreaseTimeThreshold time.Duration
	IncreaseBitrateChange int
	DecreaseBitrateChange int
	BitrateControlBuckets *BitrateControlBucketsConfig
}

type rateControllerBuckets struct {
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

	lastIncrease          time.Time
	lastDecrease          time.Time
	rateControllerOptions *RateControllerBucketsOptions

	bitrateControlBucketsManager *Manager
}

func newRateControllerBuckets(now now, initialTargetBitrate, minBitrate, maxBitrate int, rateControllerOptions *RateControllerBucketsOptions, dsw func(DelayStats)) *rateControllerBuckets {
	if rateControllerOptions == nil {
		defaultOptions := RateControllerBucketsOptions{
			IncreaseTimeThreshold: 100 * time.Millisecond,
			DecreaseTimeThreshold: 100 * time.Millisecond,
			IncreaseBitrateChange: 250000,
			DecreaseBitrateChange: 250000,
			BitrateControlBuckets: &BitrateControlBucketsConfig{
				BitrateStableThreshold:              5 * 25,
				HandleUnstableBitrateGracePeriodSec: 2,
				BitrateBucketIncrement:              250000,
				BackoffDurationsSec:                 []float64{0, 0, 15, 30, 60},
			},
		}
		rateControllerOptions = &defaultOptions
	}

	manager := NewManager(rateControllerOptions.BitrateControlBuckets)
	manager.InitializeBuckets(uint64(maxBitrate))

	return &rateControllerBuckets{
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

		rateControllerOptions: rateControllerOptions,
		lastIncrease:          time.Time{},
		lastDecrease:          time.Time{},
	}
}

func (c *rateControllerBuckets) onReceivedRate(rate int) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.latestReceivedRate = rate
}

func (c *rateControllerBuckets) updateRTT(rtt time.Duration) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.latestRTT = rtt
}

func (c *rateControllerBuckets) onDelayStats(ds DelayStats) {
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
			LatestRTT:        c.latestRTT,
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
			LatestRTT:        c.latestRTT,
		}
	}

	c.lock.Unlock()

	c.dsWriter(next)
}

func (c *rateControllerBuckets) increase(now time.Time) int {
	rate := c.target
	if time.Since(c.lastIncrease) > c.rateControllerOptions.IncreaseTimeThreshold {
		c.lastIncrease = time.Now()
		rate = c.target + c.rateControllerOptions.IncreaseBitrateChange
	}
	return rate
}

func (c *rateControllerBuckets) decrease(now time.Time) int {
	rate := c.target
	if time.Since(c.lastDecrease) > c.rateControllerOptions.DecreaseTimeThreshold {
		c.lastIncrease = time.Now()
		rate = c.target - c.rateControllerOptions.DecreaseBitrateChange
	}
	return rate
}