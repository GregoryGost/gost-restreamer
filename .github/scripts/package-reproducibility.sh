#!/bin/sh
# Повторная упаковка не должна зависеть от дат файлов нового checkout.
set -eu
: "${VERSION:?Нужна полная Git-версия}"
printf '%s\n' "$VERSION" | LC_ALL=C grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-dev\.[1-9][0-9]*\+[0-9a-f]{7,40})?$'
debian_version="$(printf '%s' "$VERSION" | sed 's/^v//; s/-dev\./~dev./')"
archive="gost-restreamer-$VERSION-linux-amd64.tar.gz"
deb="gost-restreamer_${debian_version}_amd64.deb"
sums="gost-restreamer-$VERSION-SHA256SUMS"
config=configs/example.yaml
unit=deploy/systemd/gost-restreamer.service
for path in "dist/$archive" "dist/$deb" "dist/$sums" "$config" "$unit"; do
    test -f "$path" && test -s "$path" || {
        printf 'Отсутствует исходный файл проверки: %s\n' "$path" >&2
        exit 1
    }
done

# Сохранённые данные находятся вне dist, который GoReleaser очищает при сборке.
reproduce_dir="$(mktemp -d /tmp/gost-package-reproducibility.XXXXXXXX)"
cleanup() {
    reproducibility_status=$?
    trap - EXIT HUP INT TERM
    if [ -f "$reproduce_dir/config.mtime" ]; then
        touch -r "$reproduce_dir/config.mtime" "$config" || reproducibility_status=1
    fi
    if [ -f "$reproduce_dir/unit.mtime" ]; then
        touch -r "$reproduce_dir/unit.mtime" "$unit" || reproducibility_status=1
    fi
    rm -rf "$reproduce_dir" || reproducibility_status=1
    exit "$reproducibility_status"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
touch -r "$config" "$reproduce_dir/config.mtime"
touch -r "$unit" "$reproduce_dir/unit.mtime"
cp "dist/$sums" "$reproduce_dir/SHA256SUMS"
(cd dist && sha256sum "$archive" "$deb") > "$reproduce_dir/packages.sha256"
touch -t 200001010000.00 "$config"
touch -t 200101010000.00 "$unit"

make package-debian VERSION="$VERSION"
(cd dist && sha256sum -c "$reproduce_dir/packages.sha256")
cmp "$reproduce_dir/SHA256SUMS" "dist/$sums"
printf '%s\n' 'Повторная упаковка сохранила байты .deb, .tar.gz и SHA256SUMS после изменения дат checkout.'
