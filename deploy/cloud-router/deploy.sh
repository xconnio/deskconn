#!/usr/bin/env bash
set -euo pipefail

cd -- "$(dirname -- "$0")"
release_dir=$(pwd -P)
router_name=deskconn-xconn
rollback_name=deskconn-xconn-previous
image=ubuntu:24.04
mounts=(
  --volume "$release_dir:/cloud-router:ro"
  --volume /etc/letsencrypt:/etc/letsencrypt:ro
)
exec 9>/run/lock/deskconn-xconn-deploy.lock
flock -n 9

container_is_running() {
  test "$(docker inspect --format '{{.State.Running}}' "$1" 2>/dev/null)" = true
}

wait_ready() {
  local container=$1
  local started_at=$2

  # Ready means XConn has loaded the machine realm list.
  for ((attempt = 0; attempt < 40; attempt++)); do
    if ! container_is_running "$container"; then
      break
    fi
    if docker logs --since "$started_at" "$container" 2>&1 | grep -Fx 'Ready' >/dev/null; then
      return 0
    fi
    sleep 1
  done
  docker logs --since "$started_at" "$container" >&2
  return 1
}

# The old Hub and XConn use the same host ports.
if systemctl is-active --quiet deskconn-hub.service; then
  echo 'The legacy Hub is active. Switch to XConn before using this workflow.' >&2
  exit 1
fi

# The manual cutover used the trial container name.
current_name=$router_name
if ! container_is_running "$current_name"; then
  current_name=deskconn-xconn-trial
fi
if ! container_is_running "$current_name"; then
  echo 'No running XConn container to roll back to.' >&2
  exit 1
fi

# Validate the config before stopping the running router.
docker pull "$image"
image_id=$(docker image inspect --format '{{.Id}}' "$image")
docker run --rm --network none "${mounts[@]}" --entrypoint /cloud-router/xconn "$image_id" \
  router validate --config /cloud-router/router.yaml

if docker container inspect "$rollback_name" >/dev/null 2>&1; then
  docker rm "$rollback_name"
fi
current_id=$(docker inspect --format '{{.Id}}' "$current_name")
candidate_id=$(docker create \
  --network deskconn --network-alias deskconn-cloud-router --network-alias deskconn-hub \
  --publish 8081:8081/udp \
  --publish 8082:8082/udp \
  --publish 127.0.0.1:8084:8084 \
  "${mounts[@]}" --entrypoint /cloud-router/xconn "$image_id" \
  router start --config /cloud-router/router.yaml)

restore_previous_router() {
  local deploy_status=$1
  trap - EXIT
  if ((deploy_status == 0)); then
    return
  fi

  set +e
  echo 'Deployment failed. Restoring the previous router.' >&2
  docker rm -f "$candidate_id"
  if [[ "$(docker inspect --format '{{.Name}}' "$current_id")" == "/$rollback_name" ]]; then
    docker rename "$current_id" "$current_name"
  fi
  if container_is_running "$current_id"; then
    exit "$deploy_status"
  fi

  local started_at
  started_at=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
  if docker start "$current_id" && wait_ready "$current_id" "$started_at"; then
    echo 'Previous router is ready.' >&2
  else
    echo 'Previous router did not reach Ready.' >&2
  fi
  exit "$deploy_status"
}
trap 'restore_previous_router "$?"' EXIT
trap 'exit 1' HUP INT TERM

docker stop -t 3 "$current_id"
docker rename "$current_id" "$rollback_name"
docker rename "$candidate_id" "$router_name"
started_at=$(date -u +%Y-%m-%dT%H:%M:%S.%NZ)
docker start "$router_name"
wait_ready "$router_name" "$started_at"
docker update --restart unless-stopped "$router_name"
echo 'Deskconn Cloud Router is ready.'
