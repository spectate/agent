package http

import (
	"github.com/getsentry/sentry-go"
	"github.com/go-resty/resty/v2"
	"github.com/spectate/agent/internal/logger"
	"github.com/spectate/agent/internal/version"
	"github.com/spectate/agent/pkg/proto/pb"
	"github.com/spf13/viper"
	"google.golang.org/protobuf/proto"
)

var ApiBaseUrl = "http://localhost:8000/agent-api"

type Client struct {
	client *resty.Client
}

func NewClient() *Client {
	logger.Log.Info().Msg("Initializing HTTP client")

	client := resty.New()
	client.SetBaseURL(ApiBaseUrl)
	client.SetHeader("Content-Type", "application/json")
	client.SetHeader("Accept", "application/json")
	client.SetHeader("User-Agent", "Spectated/"+version.Version+" (Build date: "+version.BuildDate+")")

	tokenValue := viper.Get("host.token")
	token, tokenOk := tokenValue.(string)

	if tokenOk && token != "" {
		client.SetHeader("X-Spectated-Token", token)
	}

	logger.Log.Info().Msg("Finished initializing HTTP client")

	return &Client{
		client: client,
	}
}

type Authorize struct {
	Token    string `json:"token"`
	Hostname string `json:"hostname"`
}

type AuthorizeSuccess struct {
	Token string `json:"token"`
}

func (http *Client) Authorize(payload Authorize) (*resty.Response, error) {
	resp, err := http.client.R().
		SetBody(payload).
		SetResult(&AuthorizeSuccess{}).
		Post("/host/authorize")

	if err != nil {
		sentry.CaptureException(err)
		return nil, err
	}

	return resp, nil
}

func (http *Client) Payload(payload *pb.MetricsPayload) (*resty.Response, error) {
	data, err := proto.Marshal(payload)
	if err != nil {
		return nil, err
	}

	resp, err := http.client.R().
		SetHeader("Content-Type", "application/x-protobuf").
		SetBody(data).
		Post("/host/ingest")

	if err != nil {
		return nil, err
	}

	if resp.IsError() {
		sentry.CaptureMessage("Error sending payload: " + resp.String())
		logger.Log.Error().Msgf("Error sending payload: %s", resp.String())
	}

	return resp, nil
}
