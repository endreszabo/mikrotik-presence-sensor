# Read-only account for the REST API (RouterOS 7.1+).
# Restrict "address" to the host running mikrotik-presence-sensor.
/user group add name=presence policy=read,api,rest-api,!write,!policy,!sensitive comment="mikrotik-presence-sensor"
/user add name=presence group=presence password="CHANGE-ME" address=2001:db8:0:10::5/128 comment="mikrotik-presence-sensor"
