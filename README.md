# Spectate Agent

Spectate Agent (`spectated`) is a service that allows you to monitor infrastructure using Spectate. This repository contains its source code.

[![License: Apache License 2.0](https://img.shields.io/badge/License-Apache%20License%202.0-yellow.svg)](https://opensource.org/license/apache-2-0/)

## Installation

Run the install-latest.sh script from the root of this repository:
```bash
chmod +x install-latest.sh
./install-latest.sh
```

## Usage

For *nix systems, run the following command:
```bash
sudo systemctl start spectated
# to stop the service, run:
sudo systemctl stop spectated
```

For MacOS, run the following command:
```bash
launchctl load -w ~/Library/LaunchAgents/Spectated.plist
# to stop the service, run:
launchctl unload -w ~/Library/LaunchAgents/Spectated.plist
```

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details
