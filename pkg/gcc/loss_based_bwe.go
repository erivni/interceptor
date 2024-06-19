// SPDX-FileCopyrightText: 2023 The Pion community <https://pion.ly>
// SPDX-License-Identifier: MIT

package gcc

import (
	"math"
	"sync"
	"time"

	"github.com/pion/interceptor/internal/cc"
	"github.com/pion/logging"
)

// LossStats contains internal statistics of the loss based controller
type LossStats struct {
	TargetBitrate int
	AverageLoss   float64
}

type lossBasedBandwidthEstimator struct {
	lock           sync.Mutex
	maxBitrate     int
	minBitrate     int
	bitrate        int
	averageLoss    float64
	lastLossUpdate time.Time
	lastIncrease   time.Time
	lastDecrease   time.Time
	options        LossBasedBandwidthEstimatorOptions
	log            logging.LeveledLogger
}

type LossBasedBandwidthEstimatorOptions struct {
	IncreaseLossThreshold float64
	IncreaseTimeThreshold time.Duration
	IncreaseBitrateChange int
	DecreaseLossThreshold float64
	DecreaseTimeThreshold time.Duration
	DecreaseBitrateChange int
}

func newLossBasedBWE(initialBitrate int, minBitrate int, maxBitrate int, options *LossBasedBandwidthEstimatorOptions) *lossBasedBandwidthEstimator {
	if options == nil {
		// constants from
		// https://datatracker.ietf.org/doc/html/draft-ietf-rmcat-gcc-02#section-6
		defaultOptions := LossBasedBandwidthEstimatorOptions{
			IncreaseLossThreshold: 0.02,
			IncreaseTimeThreshold: 200 * time.Millisecond,
			IncreaseBitrateChange: 250000,
			DecreaseLossThreshold: 0.1,
			DecreaseTimeThreshold: 200 * time.Millisecond,
			DecreaseBitrateChange: 250000,
		}
		options = &defaultOptions
	}

	return &lossBasedBandwidthEstimator{
		lock:           sync.Mutex{},
		maxBitrate:     maxBitrate,
		minBitrate:     minBitrate,
		bitrate:        initialBitrate,
		averageLoss:    0,
		lastLossUpdate: time.Time{},
		lastIncrease:   time.Time{},
		lastDecrease:   time.Time{},
		options:        *options,
		log:            logging.NewDefaultLoggerFactory().NewLogger("gcc_loss_controller"),
	}
}

func (e *lossBasedBandwidthEstimator) getEstimate(wantedRate int) LossStats {
	e.lock.Lock()
	defer e.lock.Unlock()

	if e.bitrate <= 0 {
		e.bitrate = clampInt(wantedRate, e.minBitrate, e.maxBitrate)
	}
	e.bitrate = minInt(wantedRate, e.bitrate)

	return LossStats{
		TargetBitrate: e.bitrate,
		AverageLoss:   e.averageLoss,
	}
}

func (e *lossBasedBandwidthEstimator) updateLossEstimate(results []cc.Acknowledgment) {
	if len(results) == 0 {
		return
	}

	packetsLost := 0
	for _, p := range results {
		if p.Arrival.IsZero() {
			packetsLost++
		}
	}

	e.lock.Lock()
	defer e.lock.Unlock()

	lossRatio := float64(packetsLost) / float64(len(results))
	e.averageLoss = e.average(time.Since(e.lastLossUpdate), e.averageLoss, lossRatio)
	e.lastLossUpdate = time.Now()

	increaseLoss := math.Max(e.averageLoss, lossRatio)
	decreaseLoss := math.Min(e.averageLoss, lossRatio)

	if increaseLoss < e.options.IncreaseLossThreshold && time.Since(e.lastIncrease) > e.options.IncreaseTimeThreshold {
		e.log.Infof("loss controller increasing; averageLoss: %v, decreaseLoss: %v, increaseLoss: %v", e.averageLoss, decreaseLoss, increaseLoss)
		e.lastIncrease = time.Now()
		e.bitrate = clampInt(int(e.bitrate+e.options.IncreaseBitrateChange), e.minBitrate, e.maxBitrate)
	} else if decreaseLoss > e.options.DecreaseLossThreshold && time.Since(e.lastDecrease) > e.options.DecreaseTimeThreshold {
		e.log.Infof("loss controller decreasing; averageLoss: %v, decreaseLoss: %v, increaseLoss: %v", e.averageLoss, decreaseLoss, increaseLoss)
		e.lastDecrease = time.Now()
		e.bitrate = clampInt(int(e.bitrate-e.options.DecreaseBitrateChange), e.minBitrate, e.maxBitrate)
	}
}

func (e *lossBasedBandwidthEstimator) average(delta time.Duration, prev, sample float64) float64 {
	return sample + math.Exp(-float64(delta.Milliseconds())/200.0)*(prev-sample)
}
