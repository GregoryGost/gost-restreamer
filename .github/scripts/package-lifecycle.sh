#!/bin/sh
set -eu

fail() {
    printf '%s\n' "$*" >&2
    exit 1
}

# Установка допустима только в одноразовом контейнере CI без действующего systemd.
[ "$#" -eq 2 ] && [ "$1" = --disposable-ci ] || fail 'Нужны --disposable-ci и путь к .deb.'
[ "${GITHUB_ACTIONS:-}" = true ] || fail 'Проверка разрешена только в GitHub Actions.'
[ "$(id -u)" -eq 0 ] || fail 'В одноразовом контейнере требуется root.'
[ -r /etc/os-release ] || fail 'Не удалось определить Debian.'
. /etc/os-release
[ "$ID" = debian ] && [ "$VERSION_ID" = 13 ] || fail 'Нужен одноразовый контейнер Debian 13.'
[ "$(dpkg --print-architecture)" = amd64 ] || fail 'Нужен Debian amd64.'
[ ! -e /run/systemd/system ] || fail 'Проверка не должна управлять действующим systemd.'

package=$2
[ -f "$package" ] || fail 'Файл пакета отсутствует.'
[ "$(dpkg-deb --field "$package" Package)" = gost-restreamer ] || fail 'Неверное имя пакета.'
[ "$(dpkg-deb --field "$package" Architecture)" = amd64 ] || fail 'Неверная архитектура пакета.'
if dpkg-query --show gost-restreamer >/dev/null 2>&1; then
    fail 'В контейнере уже есть запись пакета gost-restreamer.'
fi
for target in /opt/gost-restreamer /etc/gost-restreamer /var/lib/gost-restreamer /run/gost-restreamer \
    /etc/systemd/system/gost-restreamer.service /lib/systemd/system/gost-restreamer.service \
    /usr/lib/systemd/system/gost-restreamer.service; do
    [ ! -e "$target" ] && [ ! -L "$target" ] || fail "Обнаружен существующий путь: $target"
done

work=$(mktemp -d)
trap 'rm -rf -- "$work"' EXIT HUP INT TERM
dpkg-deb --raw-extract "$package" "$work/original"
grep -Fx /etc/gost-restreamer/config.yaml "$work/original/DEBIAN/conffiles" >/dev/null || fail 'Конфигурация не объявлена conffile.'

check_mode() {
    [ "$(stat -c '%a:%u:%g' "$1")" = "$2:0:0" ] || fail "Неверные права или владелец: $1"
}

# --force-depends используется только здесь: FFmpeg и systemd в тестовый контейнер не устанавливаются.
dpkg --force-depends --install "$package"
[ "$(dpkg-query --show --showformat='${Status}' gost-restreamer)" = 'install ok installed' ] || fail 'Первичная установка не завершена.'
for binary in gost-restreamer gost-process-helper; do
    cmp "$work/original/opt/gost-restreamer/$binary" "/opt/gost-restreamer/$binary"
    check_mode "/opt/gost-restreamer/$binary" 755
done
check_mode /etc/gost-restreamer/config.yaml 600
check_mode /etc/gost-restreamer 700
check_mode /etc/systemd/system/gost-restreamer.service 644
cmp "$work/original/etc/gost-restreamer/config.yaml" /etc/gost-restreamer/config.yaml
cmp "$work/original/etc/systemd/system/gost-restreamer.service" /etc/systemd/system/gost-restreamer.service

printf '\n# Пользовательское изменение конфигурации.\n' >> /etc/gost-restreamer/config.yaml
cp /etc/gost-restreamer/config.yaml "$work/user-config"
printf '\n# Пользовательская настройка unit.\n' >> /etc/systemd/system/gost-restreamer.service
cp /etc/systemd/system/gost-restreamer.service "$work/user-unit"
# Каталог в рабочей системе создаёт StateDirectory; здесь systemd не запускается.
install -d -m 0700 /var/lib/gost-restreamer
mkdir /var/lib/gost-restreamer/media
printf 'synthetic-sqlite-data\n' > /var/lib/gost-restreamer/state.sqlite
printf 'synthetic-media-data\n' > /var/lib/gost-restreamer/media/sentinel
cp /var/lib/gost-restreamer/state.sqlite "$work/state"
cp /var/lib/gost-restreamer/media/sentinel "$work/media"

# Копия с изменёнными metadata/default проверяет dpkg, а не новую сборку бинарников.
cp -a "$work/original" "$work/upgrade"
old_version=$(dpkg-deb --field "$package" Version)
upgrade_version="${old_version}+lifecycle.1"
dpkg --compare-versions "$upgrade_version" gt "$old_version" || fail 'Тестовая версия не выше исходной.'
sed -i "s/^Version: .*/Version: $upgrade_version/" "$work/upgrade/DEBIAN/control"
printf '\n# Изменённое значение поставки для проверки conffile.\n' >> "$work/upgrade/etc/gost-restreamer/config.yaml"
printf '\n# Обновлённый unit тестовой копии пакета.\n' >> "$work/upgrade/etc/systemd/system/gost-restreamer.service"
rm -f "$work/upgrade/DEBIAN/md5sums"
dpkg-deb --build --root-owner-group "$work/upgrade" "$work/upgrade.deb"
dpkg --force-depends --force-confold --install "$work/upgrade.deb"
[ "$(dpkg-query --show --showformat='${Version}' gost-restreamer)" = "$upgrade_version" ] || fail 'Обновление не установило тестовую версию.'
cmp "$work/user-config" /etc/gost-restreamer/config.yaml
cmp "$work/state" /var/lib/gost-restreamer/state.sqlite
cmp "$work/media" /var/lib/gost-restreamer/media/sentinel
cmp "$work/user-unit" /etc/systemd/system/gost-restreamer.service
check_mode /etc/gost-restreamer/config.yaml 600
check_mode /var/lib/gost-restreamer 700

dpkg --remove gost-restreamer
cmp "$work/user-config" /etc/gost-restreamer/config.yaml
cmp "$work/state" /var/lib/gost-restreamer/state.sqlite
cmp "$work/media" /var/lib/gost-restreamer/media/sentinel
for binary in gost-restreamer gost-process-helper; do
    [ ! -e "/opt/gost-restreamer/$binary" ] || fail 'Удаление сохранило бинарник.'
done
cmp "$work/user-unit" /etc/systemd/system/gost-restreamer.service

dpkg --purge gost-restreamer
[ ! -e /etc/gost-restreamer/config.yaml ] || fail 'Purge сохранил conffile.'
[ ! -e /etc/systemd/system/gost-restreamer.service ] || fail 'Purge сохранил unit conffile.'
cmp "$work/state" /var/lib/gost-restreamer/state.sqlite
cmp "$work/media" /var/lib/gost-restreamer/media/sentinel
printf '%s\n' 'Установка, обновление, remove и purge проверены на синтетических данных без запуска сервиса.'
