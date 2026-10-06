#!/usr/bin/env bash
set -euo pipefail
# Fixtures for the EC2 lifecycle vectors: the networks the VPC, subnet,
# route, route table, association, security group, internet gateway and
# gateway attachment vectors name, all free, all in us-east-1 and tagged
# kraai:fixture=ec2-lifecycle. up writes the KRAAI_LIFECYCLE_* exports the
# vectors read to $ENV_OUT; down deletes everything tagged.
#
#   dev/ec2-fixtures.sh up   [--apply]
#   dev/ec2-fixtures.sh down [--apply]
#
# Dry run unless --apply. Source the env file, then run the lifecycle with
# KRAAI_ALLOW_MUTATE=1, without which it skips every type and records
# nothing. Run down from a trap by absolute path, so a failed run still
# tears the fixtures down.

usage() { echo "usage: $0 up|down [--apply]" >&2; exit 2; }
[ $# -ge 1 ] || usage
mode="$1"
apply=false
[ "${2:-}" = "--apply" ] && apply=true
region=us-east-1
tag='Key=kraai:fixture,Value=ec2-lifecycle'
env_out="${ENV_OUT:-${TMPDIR:-/tmp}/kraai-ec2-fixtures.env}"

run() {
  if $apply; then "$@"; else echo "would run: $*" >&2; echo "dry-run"; fi
}
spec() { echo "ResourceType=$1,Tags=[{$tag}]"; }
tagged() {
  aws ec2 "$1" --region "$region" --filters Name=tag:kraai:fixture,Values=ec2-lifecycle --query "$2" --output text
}

up() {
  if [ -n "$(tagged describe-vpcs 'Vpcs[].VpcId')" ]; then
    echo "fixtures already exist; run down first" >&2
    exit 1
  fi
  # The subnet vector creates 10.99.5.0/24 with an IPv6 /64 in this VPC.
  vpc=$(run aws ec2 create-vpc --region "$region" --cidr-block 10.99.0.0/16 --amazon-provided-ipv6-cidr-block \
    --tag-specifications "$(spec vpc)" --query Vpc.VpcId --output text)
  attach_vpc=$(run aws ec2 create-vpc --region "$region" --cidr-block 10.97.0.0/16 \
    --tag-specifications "$(spec vpc)" --query Vpc.VpcId --output text)
  igw=$(run aws ec2 create-internet-gateway --region "$region" --tag-specifications "$(spec internet-gateway)" \
    --query InternetGateway.InternetGatewayId --output text)
  run aws ec2 attach-internet-gateway --region "$region" --internet-gateway-id "$igw" --vpc-id "$vpc" >/dev/null
  igw_a=$(run aws ec2 create-internet-gateway --region "$region" --tag-specifications "$(spec internet-gateway)" \
    --query InternetGateway.InternetGatewayId --output text)
  igw_b=$(run aws ec2 create-internet-gateway --region "$region" --tag-specifications "$(spec internet-gateway)" \
    --query InternetGateway.InternetGatewayId --output text)
  rtb=$(run aws ec2 create-route-table --region "$region" --vpc-id "$vpc" --tag-specifications "$(spec route-table)" \
    --query RouteTable.RouteTableId --output text)
  subnet=$(run aws ec2 create-subnet --region "$region" --vpc-id "$vpc" --cidr-block 10.99.3.0/24 \
    --tag-specifications "$(spec subnet)" --query Subnet.SubnetId --output text)
  peer=$(run aws ec2 create-security-group --region "$region" --vpc-id "$vpc" --group-name kraai-lifecycle-peer \
    --description "kraai lifecycle peer group" --tag-specifications "$(spec security-group)" --query GroupId --output text)
  if ! $apply; then
    return
  fi

  # The VPC's /56 arrives shortly after the VPC; the subnet vector takes
  # its /64 ending 05.
  ipv6=""
  for _ in $(seq 1 30); do
    # shellcheck disable=SC2016 # the backticks are a JMESPath literal, not a command
    ipv6=$(aws ec2 describe-vpcs --region "$region" --vpc-ids "$vpc" \
      --query 'Vpcs[0].Ipv6CidrBlockAssociationSet[?Ipv6CidrBlockState.State==`associated`].Ipv6CidrBlock|[0]' --output text)
    [ "$ipv6" != "None" ] && break
    sleep 2
  done
  case "$ipv6" in
  *00::/56) base="${ipv6%::/56}" ;;
  *) echo "the VPC has no IPv6 /56 ending 00 (got $ipv6)" >&2; exit 1 ;;
  esac

  cat >"$env_out" <<EOF
export KRAAI_LIFECYCLE_VPC_ID=$vpc
export KRAAI_LIFECYCLE_ATTACH_VPC_ID=$attach_vpc
export KRAAI_LIFECYCLE_INTERNET_GATEWAY_ID=$igw
export KRAAI_LIFECYCLE_INTERNET_GATEWAY_ID_A=$igw_a
export KRAAI_LIFECYCLE_INTERNET_GATEWAY_ID_B=$igw_b
export KRAAI_LIFECYCLE_ROUTE_TABLE_ID=$rtb
export KRAAI_LIFECYCLE_SUBNET_ID_C=$subnet
export KRAAI_LIFECYCLE_PEER_GROUP_ID=$peer
export KRAAI_LIFECYCLE_SUBNET_IPV6_CIDR=${base%??}05::/64
EOF
  echo "wrote $env_out" >&2
}

down() {
  local id v
  for id in $(tagged describe-security-groups 'SecurityGroups[].GroupId'); do
    run aws ec2 delete-security-group --region "$region" --group-id "$id" >/dev/null
  done
  for id in $(tagged describe-subnets 'Subnets[].SubnetId'); do
    run aws ec2 delete-subnet --region "$region" --subnet-id "$id" >/dev/null
  done
  for id in $(tagged describe-route-tables 'RouteTables[].RouteTableId'); do
    run aws ec2 delete-route-table --region "$region" --route-table-id "$id" >/dev/null
  done
  for id in $(tagged describe-internet-gateways 'InternetGateways[].InternetGatewayId'); do
    for v in $(aws ec2 describe-internet-gateways --region "$region" --internet-gateway-ids "$id" \
      --query 'InternetGateways[0].Attachments[].VpcId' --output text); do
      run aws ec2 detach-internet-gateway --region "$region" --internet-gateway-id "$id" --vpc-id "$v" >/dev/null
    done
    run aws ec2 delete-internet-gateway --region "$region" --internet-gateway-id "$id" >/dev/null
  done
  for id in $(tagged describe-vpcs 'Vpcs[].VpcId'); do
    run aws ec2 delete-vpc --region "$region" --vpc-id "$id" >/dev/null
  done
  echo "tagged VPCs left: $(tagged describe-vpcs 'Vpcs[].VpcId')" >&2
}

case "$mode" in
up) up ;;
down) down ;;
*) usage ;;
esac
