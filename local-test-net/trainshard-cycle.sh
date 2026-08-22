#!/usr/bin/env bash
# Drives training runs end to end against a local network that is already up: the hosts offer their
# nodes, governance allows the run, the chain reserves the nodes, and the coordinator builds the mesh
# and runs the container. Bring the network up first, with the genesis a training run needs:
#
#   GENESIS_OVERRIDES_JSON=$PWD/trainshard-genesis-overrides.json REST_API_ACTIVE=true ./launch.sh
#
# Then run every scenario, or name the ones you want:
#
#   ./trainshard-cycle.sh                 # run kick expire access isolation quota restart crash
#   MODE=docker ./trainshard-cycle.sh run
set -euo pipefail

cd "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

HOSTS=${HOSTS:-"join1 join2"}
CREATOR=${CREATOR:-genesis}
CHAIN_ID=${CHAIN_ID:-gonka-mainnet}
# memory keeps the whole machine in the daemon's head. docker gives it the real one: containers,
# sandboxes, wireguard and a ruleset, with only the cards and the disk quota faked
MODE=${MODE:-memory}
if [ "$MODE" = "docker" ]; then
  # the run has to be built on the base the proposal named, and the base itself counts as built on it,
  # so one image stands for both and the command is what makes the run a run
  BASE_IMAGE=${BASE_IMAGE:-alpine@sha256:48b0309ca019d89d40f670aa1bc06e426dc0931948452e8491e3d65087abc07d}
  RUN_IMAGE=${RUN_IMAGE:-$BASE_IMAGE}
  # a run that is asked to stop is expected to finish, so it takes the signal rather than being killed
  RUN_COMMAND=(sh -c 'trap "exit 0" TERM; echo training; echo trained > /workspace/result.txt; sleep 300 & wait')
  COMPOSE=(-f docker-compose.trainshard.yml -f docker-compose.trainshard-docker.yml)
else
  BASE_IMAGE=${BASE_IMAGE:-"gonka/train-base@sha256:$(printf 'a%.0s' $(seq 64))"}
  RUN_IMAGE=${RUN_IMAGE:-"gonka/train-run@sha256:$(printf 'b%.0s' $(seq 64))"}
  RUN_COMMAND=(python train.py)
  COMPOSE=(-f docker-compose.trainshard.yml)
fi
KEYRING="--keyring-backend test --keyring-dir /root/.inference"
POC_MODEL=${POC_MODEL:-Qwen/Qwen2.5-7B-Instruct}
CTL=../trainshard/build/trainshardctl

step() { printf '\n=== %s\n' "$1"; }

failures=0
check() {
  if [ "$2" = "$3" ]; then
    printf '  ok   %s: %s\n' "$1" "$2"
  else
    printf '  FAIL %s: got %s, want %s\n' "$1" "$2" "$3"
    failures=$((failures + 1))
  fi
}

# a joining node registers itself under its ml key, the genesis node under the account it was
# created with, so the key a host signs with is not always named after the host
keyname() { case $1 in genesis) echo genesis ;; *) echo "$1-WARM" ;; esac; }

address() { docker exec "$1-node" inferenced keys show "$(keyname "$1")" -a $KEYRING; }

port() { case $1 in genesis) echo 9701 ;; join1) echo 9711 ;; join2) echo 9721 ;; esac; }

query() { docker exec genesis-node inferenced query "$@" -o json; }

# tx waits for the block that holds it: the chain answers a broadcast before it runs the message, and
# the next transaction from the same account needs the sequence that run leaves behind
tx() {
  local from=$1
  shift
  local sent hash landed
  # the account the cli reads a sequence from can be a block behind the one it sends to, and the
  # chain answers that with a mismatch it will take on the next try
  for _ in 1 2 3; do
    sent=$(docker exec "$from-node" inferenced tx "$@" \
      --from "$(keyname "$from")" $KEYRING --chain-id "$CHAIN_ID" \
      --gas "${GAS:-auto}" --gas-adjustment 1.5 --gas-prices 0ngonka --yes -o json 2>&1 | tail -1)
    hash=$(echo "$sent" | jq -r '.txhash // empty' 2> /dev/null || true)
    [ -n "$hash" ] && break
    case $sent in *"account sequence mismatch"*) sleep 3 ;; *) break ;; esac
  done
  if [ -z "$hash" ] || [ "$(echo "$sent" | jq -r '.code')" != "0" ]; then
    echo "$from could not send: $sent" >&2
    return 1
  fi

  for _ in $(seq 40); do
    landed=$(docker exec "$from-node" inferenced query tx "$hash" -o json 2> /dev/null) || { sleep 1; continue; }
    if [ "$(echo "$landed" | jq -r '.code')" != "0" ]; then
      echo "$from was refused: $(echo "$landed" | jq -r '.raw_log')" >&2
      return 1
    fi
    echo "$landed"
    return 0
  done
  echo "$from sent $hash and the chain never took it" >&2
  return 1
}

hardware() {
  query inference hardware-nodes-all | jq -c --arg p "$1" '.nodes[] | select(.participant == $p) | .hardware_nodes[0].hardware'
}

# a daemon lends the node its own dapi serves inference from, under the id the chain already knows
node_of() {
  query inference hardware-nodes-all | jq -r --arg p "$1" '.nodes[] | select(.participant == $p) | .hardware_nodes[0].local_id'
}

# the chain names a node's cards the way it groups them: types upper case, sorted, each with its count
profile() { jq -r 'group_by(.type | ascii_upcase) | map("\(.[0].type | ascii_upcase) x\(map(.count) | add)") | sort | join(" | ")'; }

# a daemon reports the cards it holds, and the chain must already say the same, so it takes the
# first kind and the total count, the way the chain adapter reads them back
model() { jq -r '.[0].type'; }
count() { jq -r 'map(.count) | add'; }

gov_authority() {
  query auth module-account gov | jq -r '.account.value.address // .account.address // .account.base_account.address'
}

# once the first epoch is in, the stake is split between the participants, so a proposal needs every
# vote to reach quorum, and a host that has never been paid cannot pay for its own
fund() {
  if [ "$(query bank balances "$(address "$1")" | jq -r '.balances | length')" != "0" ]; then
    return
  fi
  GAS=200000 tx genesis bank send "$(address genesis)" "$(address "$1")" 100000000ngonka > /dev/null
  sleep 6
}

# pass puts a proposal through: submit, vote it in, wait out the period, and answer with its id
pass() {
  docker cp "$1" "$CREATOR-node:/tmp/proposal.json" > /dev/null
  tx "$CREATOR" gov submit-proposal /tmp/proposal.json > /dev/null
  sleep 6

  local id status
  id=$(query gov proposals | jq -r '.proposals[-1].id')
  # every stake has to vote to reach quorum, and the period is short, so they vote at once. A vote
  # also costs a little more than the chain estimates for it
  for host in genesis $HOSTS; do
    GAS=300000 tx "$host" gov vote "$id" yes > /dev/null &
  done
  wait

  for _ in $(seq 40); do
    status=$(query gov proposal "$id" | jq -r '.proposal.status')
    [ "$status" = "PROPOSAL_STATUS_VOTING_PERIOD" ] || break
    sleep 3
  done
  echo "proposal $id $status" >&2
  [ "$status" = "PROPOSAL_STATUS_PASSED" ] || return 1
  echo "$id"
}

# the run can only ask for a profile governance has allowed, and a mock node only does the proof of
# compute the params name a model for, so put both there if they are not
allow_profile() {
  local wanted=$1
  if query inference params | jq -e --arg p "$wanted" '.params.training_params.allowed_gpu_profile_ids | index($p)' > /dev/null; then
    echo "$wanted is already allowed"
    return
  fi

  query inference params | jq --arg a "$(gov_authority)" --arg p "$wanted" --arg m "$POC_MODEL" '{
    messages: [{"@type": "/inference.inference.MsgUpdateParams", authority: $a,
      params: (.params
        | .poc_params.models[0].model_id = $m
        # the api commits its proof of compute off chain, which the chain only takes in v2
        | .poc_params.poc_v2_enabled = true
        | .poc_params.confirmation_poc_v2_enabled = true
        | .training_params.allowed_gpu_profile_ids += [$p]
        # a settled shard is kept two epochs plus the buffer, which the chain checks against its own
        # epoch length rather than the default this net was built with
        | .training_params.settled_shard_retention_blocks = "300")}],
    metadata: "trainshard", deposit: "1000000ngonka",
    title: "a gpu profile for training", summary: "let a run ask for the cards these hosts hold"
  }' > /tmp/trainshard-params.json
  pass /tmp/trainshard-params.json > /dev/null
  query inference params | jq -c '.params.training_params.allowed_gpu_profile_ids'
}

# newest_open answers with the training proposal still waiting to be assembled: the chain hands them
# out one by one and only lets them be looked up by id
newest_open() {
  local id=1 held last=''
  while true; do
    held=$(query inference show-trainshard-proposal "$id" 2> /dev/null | jq -c '.proposal // empty' 2> /dev/null || true)
    [ -n "$held" ] || break
    [ "$(echo "$held" | jq -r '.status')" = "TRAINSHARD_PROPOSAL_STATUS_OPEN" ] && last=$id
    id=$((id + 1))
  done
  echo "$last"
}

shard() { query inference show-trainshard "$1" | jq -c '.trainshard'; }

# node_status answers what the shard says about one host's node, which is how a kick is read back. It
# gives the chain a few blocks to say it, since the query is answered from the last one committed
node_status() {
  local status
  for _ in $(seq 10); do
    status=$(shard "$1" | jq -r --arg p "$2" '.nodes[] | select(.participant == $p) | .status')
    [ "$status" = TRAINSHARD_NODE_STATUS_ACTIVE ] || break
    sleep 2
  done
  echo "$status"
}

# open_shard asks governance for a run of its own and has the chain reserve the nodes for it
open_shard() {
  local blocks=$1 id
  jq -n --arg a "$(gov_authority)" --arg c "$(address "$CREATOR")" --arg p "$GPU_PROFILE" \
    --arg image "$BASE_IMAGE" --arg blocks "$blocks" \
    --argjson nodes "$(echo "$HOSTS" | wc -w | tr -d ' ')" '{
    messages: [{
      "@type": "/inference.inference.MsgCreateTrainshardProposal",
      authority: $a, creator: $c, gpu_profile_id: $p,
      max_nodes: $nodes, max_duration_blocks: $blocks, base_image: $image, run_key: ""
    }],
    metadata: "trainshard", deposit: "1000000ngonka",
    title: "a training run", summary: "lend gpus for one training run"
  }' > /tmp/trainshard-proposal.json
  pass /tmp/trainshard-proposal.json > /dev/null

  # the chain keeps a node out of every run it could fill, so how many it can lend depends on how many
  # took part in the epoch that is running: an epoch short of nodes is worth waiting out
  id=$(newest_open)
  local try
  for try in $(seq 8); do
    tx "$CREATOR" inference assemble-trainshard "$id" > /dev/null && break
    [ "$try" = 8 ] && return 1
    echo "waiting for an epoch with room for $(nodes) nodes" >&2
    sleep 45
  done
  sleep 5

  local held
  held=$(query inference active-trainshards | jq -r '(.trainshards // [])[-1] | .trainshard_id // .trainshardId')
  if [ -z "$held" ] || [ "$held" = "null" ]; then
    echo "proposal $id was assembled and the chain holds no shard for it" >&2
    return 1
  fi
  echo "$held"
}

# settle_open closes whatever an earlier run left behind, so the nodes are free to be reserved again
settle_open() {
  local held
  for held in $(query inference active-trainshards | jq -r '(.trainshards // [])[] | .trainshard_id // .trainshardId'); do
    echo "settling shard $held from an earlier run"
    tx "$CREATOR" inference settle-trainshard "$held" > /dev/null || true
  done
  sleep 10
}

# serving reads what the participant's own dapi says about the node it lends: a node that is back is
# one the dapi is free to send inference to again
serving() {
  docker exec "$1-trainshardd" wget -qO- "http://$1-api:9200/admin/v1/nodes" |
    jq -r '.[0].state.admin_state.enabled'
}

# the engine hands a run its cards through the device interface a driver installs, and no driver is
# installed here, so the testbed registers a device under that name which adds nothing to the run
fake_gpus() {
  docker run --rm -i --privileged --pid=host alpine:3.21 \
    nsenter -t 1 -m -- sh -c 'mkdir -p /etc/cdi && cat > /etc/cdi/trainshard-fake-gpu.yaml' < trainshard-testbed/fake-gpu.yaml
  # the engine rereads the directory on its own clock, and a daemon that asks before it has lets its
  # opt-in lapse, so wait until a card can actually be handed out
  local try
  for try in $(seq 30); do
    docker run --rm --device nvidia.com/gpu=0 alpine:3.21 true 2> /dev/null && return
    sleep 2
  done
  echo "the engine still cannot hand out a fake card" >&2
  return 1
}

# the daemon puts a run's disk limit on an xfs project, and the machine this runs on has no xfs, so
# the testbed gives each host a small one out of a file and mounts it where its state lives
real_quota() {
  local host=$1
  if docker run --rm --privileged --pid=host alpine:3.21 \
    nsenter -t 1 -m -- grep -q " /var/lib/trainshardd/$host xfs " /proc/mounts; then
    return
  fi

  docker run --rm --privileged --pid=host -v /var/lib:/hostvar alpine:3.21 sh -c "
    apk add --no-cache xfsprogs > /dev/null 2>&1
    test -f /hostvar/trainshard-$host.img || truncate -s 4G /hostvar/trainshard-$host.img
    mkfs.xfs -q /hostvar/trainshard-$host.img > /dev/null 2>&1 || true
    nsenter -t 1 -m -- sh -c 'mkdir -p /var/lib/trainshardd/$host &&
      mount -o loop,prjquota /var/lib/trainshard-$host.img /var/lib/trainshardd/$host'"
}

# logs follow until the container ends, so a check reads the first few seconds and lets go
logs_of() {
  $CTL logs "$1" "$2" > /tmp/trainshard-logs.txt 2>&1 &
  local reader=$!
  sleep 6
  kill "$reader" 2> /dev/null || true
  wait "$reader" 2> /dev/null || true
  cat /tmp/trainshard-logs.txt
}

# the coordinator prints a table a node to a line, so a check counts the lines that say what it wants
nodes() { echo "$HOSTS" | wc -w | tr -d ' '; }
column() { awk -v col="$1" -v want="$2" 'NR > 1 && $col == want' | wc -l | tr -d ' '; }

# sessions is what an operator gets out of a running node: its output and a way in
sessions() {
  local shard_id=$1 node=$2
  check "logs come back" "$(logs_of "$shard_id" "$node" | grep -c training)" 1

  printf 'echo alive\nexit\n' | $CTL shell "$shard_id" "$node" > /tmp/trainshard-shell.txt 2>&1 || true
  # a terminal echoes what it is told, so the answer is the line the shell itself printed
  check "a shell reaches the container" "$(grep -c '^alive' /tmp/trainshard-shell.txt)" 1
}

# the run's container shares the sandbox namespace, so a probe run inside it opens exactly what the
# run itself could open
reaches() {
  if docker exec "$1" nc -w 3 "$2" "$3" < /dev/null > /dev/null 2>&1; then echo yes; else echo no; fi
}

resolves() {
  if docker exec "$1" nslookup "$2" > /dev/null 2>&1; then echo yes; else echo no; fi
}

container_of() { echo "trainshard-$1-$(node_of "$(address "$2")")"; }

address_of() { docker inspect "$1" --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}'; }

mesh_address() { docker exec "$1" ip -4 -o addr show | awk '$2 ~ /^ts/ {print $4}' | cut -d/ -f1; }

scenario_isolation() {
  step "isolation: a run reaches its mesh and what the deploy allowed, and nothing else"
  if [ "$MODE" != "docker" ]; then
    echo "only the docker machine has a network to close"
    return
  fi
  settle_open

  local first=${HOSTS%% *} second=${HOSTS##* } allowed shard_id
  allowed="$first-api"
  shard_id=$(open_shard 500)
  $CTL prepare "$shard_id" --wait 3m
  $CTL deploy "$shard_id" --image "$RUN_IMAGE" --gpus 1 --disk-bytes 1073741824 \
    --source "$allowed:9200" -- "${RUN_COMMAND[@]}"
  $CTL start "$shard_id"

  local here there peer
  here=$(container_of "$shard_id" "$first")
  there=$(container_of "$shard_id" "$second")
  peer=$(mesh_address "$there")
  echo "$here reaches for $there at $peer"

  # something has to answer on the mesh, or an open route reads the same as a closed one
  docker exec -d "$there" nc -l -p 5000
  sleep 1
  check "the mesh carries the run" "$(reaches "$here" "$peer" 5000)" yes
  check "the source the deploy named is open" "$(reaches "$here" "$allowed" 9200)" yes
  check "another port on that host is not" "$(reaches "$here" "$allowed" 9100)" no
  check "the operator network is closed" "$(reaches "$here" "$(address_of "$first-node")" 26657)" no
  check "names the run was not given do not resolve" "$(resolves "$here" gonka.ai)" no

  $CTL stop "$shard_id"
  tx "$CREATOR" inference settle-trainshard "$shard_id" > /dev/null
}

container_id() { docker inspect "$1" --format '{{.Id}}' 2> /dev/null || echo none; }

# the coordinator answers only when every host does, so asking it for the shard is asking whether
# the daemon that was restarted is back
awake() {
  local try
  for try in $(seq 30); do
    $CTL status "$1" > /dev/null 2>&1 && return
    sleep 2
  done
  echo "the hosts never answered again" >&2
  return 1
}

# a refused command answers with a message and a non-zero exit, so a check reads the exit
refused() {
  if "$@" > /dev/null 2>&1; then echo no; else echo yes; fi
}

scenario_access() {
  step "access: a host answers the one who opened the run and no one else"
  settle_open

  local first=${HOSTS%% *} shard_id stranger
  shard_id=$(open_shard 500)
  $CTL prepare "$shard_id" --wait 3m

  # a host that lends a node to this very run is still not the one who opened it
  stranger=$(docker exec "$first-node" sh -c "yes | inferenced keys export $(keyname "$first") --unarmored-hex --unsafe $KEYRING" 2> /dev/null | tail -1)
  check "the run answers its creator" "$(refused $CTL status "$shard_id")" no
  check "and no one else" "$(refused env TRAINSHARDCTL_PRIVATE_KEY="$stranger" $CTL status "$shard_id")" yes
  check "a caller that did not sign is turned away" \
    "$(refused docker exec "$first-trainshardd" wget -q -O /dev/null http://localhost:9700/trainshard/v0/readiness)" yes

  tx "$CREATOR" inference settle-trainshard "$shard_id" > /dev/null
  sleep 15
}

scenario_restart() {
  step "restart: a daemon that comes back keeps the run, and a changed run is built again"
  if [ "$MODE" != "docker" ]; then
    echo "only the docker machine keeps a run outside the daemon"
    return
  fi
  settle_open

  local first=${HOSTS%% *} shard_id here before after
  shard_id=$(open_shard 500)
  $CTL prepare "$shard_id" --wait 3m
  $CTL deploy "$shard_id" --image "$RUN_IMAGE" --gpus 1 --disk-bytes 1073741824 -- "${RUN_COMMAND[@]}"
  $CTL start "$shard_id"

  here=$(container_of "$shard_id" "$first")
  before=$(container_id "$here")
  docker restart "$first-trainshardd" > /dev/null
  awake "$shard_id"
  check "the run outlives its daemon" "$(container_id "$here")" "$before"
  check "nodes still running" "$($CTL status "$shard_id" | column 2 running)" "$(nodes)"

  # the image stays the same and the run does not, so the container has to be built again
  $CTL stop "$shard_id"
  $CTL deploy "$shard_id" --image "$RUN_IMAGE" --gpus 1 --disk-bytes 1073741824 --env ROUND=2 -- "${RUN_COMMAND[@]}"
  after=$(container_id "$here")
  check "a changed run is built again" "$([ "$after" != "$before" ] && echo yes || echo no)" yes
  check "the run carries what the deploy gave it" \
    "$(docker inspect "$here" --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -c '^ROUND=2$')" 1

  tx "$CREATOR" inference settle-trainshard "$shard_id" > /dev/null
  sleep 15
}

scenario_crash() {
  step "crash: a run that dies is reported, and the node still comes back"
  if [ "$MODE" != "docker" ]; then
    echo "only the docker machine has a run to kill"
    return
  fi
  settle_open

  local first=${HOSTS%% *} shard_id here
  shard_id=$(open_shard 500)
  $CTL prepare "$shard_id" --wait 3m
  $CTL deploy "$shard_id" --image "$RUN_IMAGE" --gpus 1 --disk-bytes 1073741824 -- "${RUN_COMMAND[@]}"
  $CTL start "$shard_id"

  here=$(container_of "$shard_id" "$first")
  docker kill "$here" > /dev/null
  sleep 10
  check "the report tells the run was killed" "$($CTL report "$shard_id" | column 4 137)" 1

  $CTL stop "$shard_id" || true
  tx "$CREATOR" inference settle-trainshard "$shard_id" > /dev/null
  sleep 15
  for host in $HOSTS; do
    check "$host serves inference again" "$(serving "$host")" true
  done
}

scenario_quota() {
  step "quota: a run cannot write past the disk it was given"
  if [ "$MODE" != "docker" ]; then
    echo "only the docker machine puts the disk on a real filesystem"
    return
  fi
  settle_open

  local first=${HOSTS%% *} shard_id here used limit=33554432
  shard_id=$(open_shard 500)
  $CTL prepare "$shard_id" --wait 3m
  $CTL deploy "$shard_id" --image "$RUN_IMAGE" --gpus 1 --disk-bytes "$limit" -- "${RUN_COMMAND[@]}"
  $CTL start "$shard_id"

  here=$(container_of "$shard_id" "$first")
  check "the disk the run was given is the disk it has" "$($CTL status "$shard_id" | awk 'NR == 2 {print $7}')" "$limit"
  check "a run that writes past it is stopped" \
    "$(refused docker exec "$here" dd if=/dev/zero of=/workspace/fill bs=1M count=64)" yes

  used=$(docker exec "$here" du -sk /workspace | awk '{print $1}')
  check "and what it wrote stayed inside" "$([ "$used" -le $((limit / 1024)) ] && echo yes || echo no)" yes

  check "the run cannot write outside the disk it was given" \
    "$(refused docker exec "$here" sh -c 'echo x > /marker')" yes
  check "nor into the files the engine handed it" \
    "$(refused docker exec "$here" sh -c 'echo x >> /etc/resolv.conf')" yes
  check "its scratch is memory, not the host disk" \
    "$(docker exec "$here" sh -c 'df -t tmpfs /tmp > /dev/null && echo yes || echo no')" yes
  check "and what it prints is rolled" \
    "$(docker inspect "$here" --format '{{if .HostConfig.LogConfig.Config}}yes{{else}}no{{end}}')" yes

  $CTL stop "$shard_id"
  tx "$CREATOR" inference settle-trainshard "$shard_id" > /dev/null
  sleep 15
}

scenario_run() {
  step "run: one shard from reservation to settlement"
  settle_open
  local shard_id
  shard_id=$(open_shard 500)
  echo "shard $shard_id holds $(shard "$shard_id" | jq -c '[.nodes[].participant]')"

  $CTL prepare "$shard_id" --wait 3m
  $CTL deploy "$shard_id" --image "$RUN_IMAGE" --gpus 1 --disk-bytes 1073741824 -- "${RUN_COMMAND[@]}"
  $CTL start "$shard_id"
  local status
  status=$($CTL status "$shard_id")
  echo "$status"
  check "nodes running" "$(echo "$status" | column 2 running)" "$(nodes)"
  check "nodes on the mesh" "$(echo "$status" | column 4 true)" "$(nodes)"
  # a lent node belongs to the run alone, so its own dapi must not be sending inference to it
  for host in $HOSTS; do
    check "$host holds its node out of inference" "$(serving "$host")" false
  done
  if [ "$MODE" = "docker" ]; then sessions "$shard_id" "$(echo "$status" | awk 'NR == 2 {print $1}')"; fi

  $CTL stop "$shard_id"
  check "nodes exited cleanly" "$($CTL report "$shard_id" | column 4 0)" "$(nodes)"

  tx "$CREATOR" inference settle-trainshard "$shard_id" > /dev/null
  sleep 15
  check "shard settled" "$(shard "$shard_id" | jq -r '.status')" TRAINSHARD_STATUS_SETTLED
  for host in $HOSTS; do
    check "$host serves inference again" "$(serving "$host")" true
  done
}

scenario_kick() {
  step "kick: a host that does not come up is dropped from the run"
  settle_open
  local down=${HOSTS##* } shard_id

  # the host goes away after the chain has lent its node, since a node whose host is already gone is
  # one the chain will not lend in the first place
  shard_id=$(open_shard 500)
  docker stop "$down-trainshardd" > /dev/null
  echo "$down is down"

  $CTL prepare "$shard_id" --wait 40s || true
  check "$down was kicked" "$(node_status "$shard_id" "$(address "$down")")" TRAINSHARD_NODE_STATUS_AUTOKICKED

  docker start "$down-trainshardd" > /dev/null
  tx "$CREATOR" inference settle-trainshard "$shard_id" > /dev/null
  sleep 20
}

scenario_expire() {
  step "expire: the nodes come back on their own when the run runs out"
  settle_open
  local shard_id
  shard_id=$(open_shard 30)
  $CTL prepare "$shard_id" --wait 2m
  $CTL deploy "$shard_id" --image "$RUN_IMAGE" --gpus 1 --disk-bytes 1073741824 -- "${RUN_COMMAND[@]}"
  $CTL start "$shard_id"
  echo "shard $shard_id expires at height $(shard "$shard_id" | jq -r '.expires_at_height')"

  local at
  for _ in $(seq 60); do
    at=$(shard "$shard_id" | jq -r '.status')
    [ "$at" = "TRAINSHARD_STATUS_ACTIVE" ] || break
    sleep 3
  done
  check "shard ran out" "$at" TRAINSHARD_STATUS_EXPIRED

  sleep 20
  for host in $HOSTS; do
    check "$host serves inference again" "$(serving "$host")" true
  done
}

step "who lends what"
echo "run driven by $CREATOR $(address "$CREATOR")"
for host in $HOSTS; do
  echo "$host $(address "$host") $(hardware "$(address "$host")" | profile)"
done
GPU_PROFILE=${GPU_PROFILE:-$(hardware "$(address "${HOSTS%% *}")" | profile)}
echo "the run asks for $GPU_PROFILE"

step "training daemons"
if [ "$MODE" = "docker" ]; then fake_gpus; fi
for host in $HOSTS; do
  fund "$host"
  if [ "$MODE" = "docker" ]; then real_quota "$host"; fi
  cards=$(hardware "$(address "$host")")
  KEY_NAME=$host TRAINSHARD_KEY_NAME=$(keyname "$host") \
    TRAINSHARD_PARTICIPANT=$(address "$host") TRAINSHARD_PORT=$(port "$host") \
    TRAINSHARD_NODES=$(node_of "$(address "$host")") \
    TRAINSHARD_GPU_MODEL=$(echo "$cards" | model) TRAINSHARD_GPUS=$(echo "$cards" | count) \
    docker compose -p "$host" "${COMPOSE[@]}" up -d 2> /dev/null
done
echo "waiting for the nodes to be offered for training"
sleep 25

step "governance allows the profile"
allow_profile "$GPU_PROFILE"

step "the coordinator"
(cd ../trainshard && go build -o build/trainshardctl ./cmd/trainshardctl)
# the export asks twice whether we mean it, and the yes belongs inside the container: outside it the
# broken pipe would take the script down with it
docker exec "$CREATOR-node" sh -c "yes | inferenced keys export $(keyname "$CREATOR") --unarmored-hex --unsafe $KEYRING" 2> /dev/null |
  tail -1 > /tmp/trainshard-creator.key
{
  echo '{'
  separator=''
  for host in $HOSTS; do
    printf '%s  "%s": "http://localhost:%s"\n' "$separator" "$(address "$host")" "$(port "$host")"
    separator=','
  done
  echo '}'
} > /tmp/trainshard-hosts.json

export TRAINSHARDCTL_PRIVATE_KEY=$(cat /tmp/trainshard-creator.key)
export TRAINSHARDCTL_HOSTS=/tmp/trainshard-hosts.json
export TRAINSHARDCTL_CHAIN_GRPC=localhost:9090
export TRAINSHARDCTL_CHAIN_ID=$CHAIN_ID
echo "driving $(jq -r 'keys | length' /tmp/trainshard-hosts.json) hosts as $(address "$CREATOR")"

for scenario in ${*:-run kick expire access isolation quota restart crash}; do
  "scenario_$scenario"
done

step "result"
if [ "$failures" != "0" ]; then
  echo "$failures checks failed"
  exit 1
fi
echo "every check passed"
