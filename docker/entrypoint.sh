#!/bin/sh

echo "Distributing files"
if [ -d "/opt/cni/bin/" ] && [ -f "./meshnet" ]; then
  cp ./meshnet /opt/cni/bin/
fi

if [ -d "/etc/cni/net.d/" ] && [ -f "./meshnet.conf" ]; then
  cp ./meshnet.conf /etc/cni/net.d/
fi

if [ ! -f /etc/cni/net.d/00-meshnet.conf ]; then
  echo "Mergin existing CNI configuration with meshnet"
  existing=$(ls -1 /etc/cni/net.d/ | egrep "flannel|weave|bridge|calico|contiv|cilium|cni" | head -n1)
  jq  -s '.[1].delegate = (.[0].plugins[0])' /etc/cni/net.d/$existing /etc/cni/net.d/meshnet.conf | jq .[1] > /etc/cni/net.d/00-meshnet.conf
else
  echo "Re-using existing CNI config"
fi

echo "Starting meshnetd daemon"
/meshnetd
