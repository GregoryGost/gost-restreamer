#!/bin/sh
# Проверка выполняется в целевом Debian-контейнере после GoReleaser.
set -eu
: "${VERSION:?Нужна полная Git-версия}"
: "${MERGE_SHA:?Нужен полный commit}"
: "${DEB_VERSION:?Нужна Debian-версия}"
test "$(go env GOOS)/$(go env GOARCH)/$(go env CGO_ENABLED)" = linux/amd64/1
test "$(go env GOVERSION)" = go1.27.3
test "$(git rev-parse HEAD)" = "$MERGE_SHA"

archive="gost-restreamer-$VERSION-linux-amd64.tar.gz"
deb="gost-restreamer_${DEB_VERSION}_amd64.deb"
sums="gost-restreamer-$VERSION-SHA256SUMS"
test "$(find dist -maxdepth 1 -type f -name '*.deb' | wc -l)" -eq 1
test "$(find dist -maxdepth 1 -type f -name '*.tar.gz' | wc -l)" -eq 1
test "$(wc -l < "dist/$sums")" -eq 2
(cd dist && sha256sum --strict -c "$sums")
for artifact in "$archive" "$deb"; do
    test "$(awk -v name="$artifact" '$2 == name { n++ } END { print n+0 }' "dist/$sums")" -eq 1
done
test "$(dpkg-deb -f "dist/$deb" Package)" = gost-restreamer
test "$(dpkg-deb -f "dist/$deb" Architecture)" = amd64
test "$(dpkg-deb -f "dist/$deb" Version)" = "$DEB_VERSION"

verify_dir="$(mktemp -d "${TMPDIR:-/tmp}/gost-package-verify.XXXXXXXX")"
trap 'rm -rf "$verify_dir"' EXIT HUP INT TERM
mkdir "$verify_dir/archive" "$verify_dir/package" "$verify_dir/control"
tar -xzf "dist/$archive" -C "$verify_dir/archive"
dpkg-deb -x "dist/$deb" "$verify_dir/package"
dpkg-deb -e "dist/$deb" "$verify_dir/control"
printf '%s\n' configs/example.yaml deploy/systemd/gost-restreamer.service gost-process-helper gost-restreamer > "$verify_dir/expected"
tar -tzf "dist/$archive" | LC_ALL=C sort > "$verify_dir/actual"
cmp "$verify_dir/expected" "$verify_dir/actual"

expected="$VERSION commit=$MERGE_SHA dirty=false go=$(go env GOVERSION)"
for binary in gost-restreamer gost-process-helper; do
    path="$verify_dir/package/opt/gost-restreamer/$binary"
    test "$(stat -c '%a' "$path")" = 755
    test "$("$path" --version)" = "$expected"
    cmp "$path" "$verify_dir/archive/$binary"
    go version -m "$path" > "$verify_dir/buildinfo"
    for setting in GOOS=linux GOARCH=amd64 CGO_ENABLED=1 "vcs.revision=$MERGE_SHA" vcs.modified=false; do
        awk -v setting="$setting" '$1 == "build" && $2 == setting { found=1 } END { exit !found }' "$verify_dir/buildinfo"
    done
done
for pair in 'configs/example.yaml etc/gost-restreamer/config.yaml' 'deploy/systemd/gost-restreamer.service etc/systemd/system/gost-restreamer.service'; do
    source=${pair%% *}
    target=${pair#* }
    cmp "$source" "$verify_dir/package/$target"
    cmp "$source" "$verify_dir/archive/$source"
    awk -v path="/$target" '$1 == path { found=1 } END { exit !found }' "$verify_dir/control/conffiles"
done
test "$(stat -c '%a' "$verify_dir/package/etc/gost-restreamer")" = 700
test "$(stat -c '%a' "$verify_dir/package/etc/gost-restreamer/config.yaml")" = 600
test "$(stat -c '%a' "$verify_dir/package/etc/systemd/system/gost-restreamer.service")" = 644
for script in postinst prerm postrm; do
    test -x "$verify_dir/control/$script"
    cmp "deploy/debian/$script" "$verify_dir/control/$script"
done
# Отсутствие посторонних файлов проверяется по полному payload пакета.
find "$verify_dir/package" -type f | sed "s|^$verify_dir/package/||" | LC_ALL=C sort > "$verify_dir/actual"
printf '%s\n' etc/gost-restreamer/config.yaml etc/systemd/system/gost-restreamer.service opt/gost-restreamer/gost-process-helper opt/gost-restreamer/gost-restreamer > "$verify_dir/expected"
cmp "$verify_dir/expected" "$verify_dir/actual"

dpkg --compare-versions '0.2.0~dev.1+abc123' lt '0.2.0~dev.2+def456'
dpkg --compare-versions '0.2.0~dev.2+def456' lt '0.2.0'
dpkg --compare-versions '0.2.0~dev.10+abc123' lt '0.2.0'
dpkg --compare-versions '0.2.0' lt '0.3.0~dev.1+123abc'
printf '%s\n' 'Проверены пакет, архив, SHA256, версии, VCS и порядок Debian-версий.'
