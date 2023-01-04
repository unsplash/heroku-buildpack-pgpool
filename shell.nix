let
  # A pinned recent revision of nixpkgs/unstable.
  pkgs = import
    (builtins.fetchTarball {
      url = "https://github.com/NixOS/nixpkgs/archive/ee01de29d2f58d56b1be4ae24c24bd91c5380cea.tar.gz";
      sha256 = "0829fqp43cp2ck56jympn5kk8ssjsyy993nsp0fjrnhi265hqps7";
    })
    { };
in

pkgs.mkShell {
  buildInputs = with pkgs; [
    go
    docker
    docker-compose
    postgresql_13
  ];
}
