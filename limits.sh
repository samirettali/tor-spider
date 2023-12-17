#!/usr/bin/env bash
sudo sysctl -w kern.maxfiles=1048600
sudo sysctl -w kern.maxfilesperproc=1048576
sudo ulimit -S -n 1048576
sudo launchctl limit maxfiles 1048576 1048600
