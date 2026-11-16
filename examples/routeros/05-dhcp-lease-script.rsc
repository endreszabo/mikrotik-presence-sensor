# Optional: explicit, easy to match log lines from the DHCP server, paired
# with this custom rule in the sensor configuration:
#
#   syslog:
#     rules:
#       - name: lease-script
#         pattern: 'presence (?P<event>seen|leave) {mac}'
#         kind: dhcp
#
# Replace "dhcp1" with your DHCP server name.
/ip dhcp-server set dhcp1 lease-script={
  :if ($leaseBound = "1") do={
    :log info "presence seen $leaseActMAC"
  } else={
    :log info "presence leave $leaseActMAC"
  }
}
