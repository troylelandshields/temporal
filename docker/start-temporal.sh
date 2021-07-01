#!/bin/bash

set -ex

dockerize -template /etc/temporal/config/config_template.yaml:/etc/temporal/config/docker.yaml

# Convert semicolon (or comma for backward compatibility) separated string (i.e. "history:matching")
# to valid flags list (i.e. "--service=history --service=matching").
IFS=':,'; declare -a SERVICE_LIST=($SERVICES)
SERVICE_ARGS=$(printf -- "--service=%s " "${SERVICE_LIST[@]}")
SERVICE_ARGS="${SERVICE_ARGS%% }"
exec temporal-server --root $TEMPORAL_HOME --env docker start $SERVICE_ARGS
