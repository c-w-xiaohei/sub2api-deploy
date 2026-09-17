{ pkgs }:
{
  composition = [ pkgs.stdenvNoCC pkgs.makeWrapper pkgs.bash ];
  controller = [ pkgs.sops pkgs.openssh pkgs.coreutils ];
  host = [
    pkgs.docker pkgs.containerd pkgs.runc pkgs.nftables pkgs.iptables pkgs.openssh
    pkgs.coreutils pkgs.findutils pkgs.gnugrep pkgs.gawk pkgs.gnused
  ];
}
