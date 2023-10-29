//go:build darwin

package service

import (
	"github.com/kardianos/service"
)

var serviceConfig = &service.Config{
	Name:        "spectated",
	DisplayName: "Spectated",
	Description: "Spectate host monitoring agent",
	Arguments:   []string{"start"},
	Option: service.KeyValue{
		"UserService": true,
	},
}

func init() {}
