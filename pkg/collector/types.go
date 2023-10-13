package collector

// StrategyData contains the data generated by a strategy.
type StrategyData struct {
	Strategy  string      `json:"strategy"`
	Data      interface{} `json:"data"`
	Timestamp int64       `json:"timestamp"`
}
