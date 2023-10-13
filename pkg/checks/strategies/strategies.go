package strategies

import (
	"context"
	"github.com/spectate/agent/pkg/proto/pb"
	"time"
)

// Strategy provides a common interface for different monitoring tasks.
type Strategy interface {
	Options() Options
	Run(ctx context.Context, dataCh chan<- []*pb.MetricsPayload_Metric) error
}

type Options struct {
	Namespace string
	Id        string
	Frequency time.Duration
}

type StrategyRegistrar struct {
	Strategies map[time.Duration][]Strategy
}

var Registrar = &StrategyRegistrar{
	Strategies: make(map[time.Duration][]Strategy),
}

func (r *StrategyRegistrar) RegisterStrategy(strategy Strategy) {
	duration := strategy.Options().Frequency
	r.Strategies[duration] = append(r.Strategies[duration], strategy)
}
