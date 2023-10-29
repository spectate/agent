package service

import (
	"context"
	"github.com/kardianos/service"
	"github.com/spectate/agent/internal/logger"
	"github.com/spectate/agent/pkg/app"
)

type program struct {
	exit chan struct{}
}

func (p *program) Start(s service.Service) error {
	go p.run()
	return nil
}

func (p *program) Stop(s service.Service) error {
	close(p.exit)
	return nil
}

func (p *program) run() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	agent := app.NewApp()

	agent.Start(ctx)

	<-p.exit
}

func NewService() (service.Service, error) {
	prg := &program{exit: make(chan struct{})}

	s, err := service.New(prg, serviceConfig)
	if err != nil {
		logger.Log.Fatal().Err(err)
	}

	return s, err
}
