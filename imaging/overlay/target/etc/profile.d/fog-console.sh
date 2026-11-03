# A login shell here — on the monitor or over SSH — opens on the same facts
# the console banner shows: whether Fog is up and where to reach it.
case $- in
  *i*) [ -x /usr/local/bin/fog-console ] && /usr/local/bin/fog-console --once ;;
esac
