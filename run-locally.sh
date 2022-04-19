#!/bin/bash

export EXPORTER_ENDPOINT=apm2.us2.aws.watcher.cic2.tibcocloud.com:443
export EXPORTER_HEADERS="Authorization=Bearer APM_SECRET_TOKEN"

go run main.go
