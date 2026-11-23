package youtube

import (
	"sync"
	"time"
)

const (
	// searchDailyLimit содержит дневное число страниц поиска по умолчанию у Google.
	searchDailyLimit = 100
	// videoDailyLimit ограничивает вклад обнаружения в общую суточную квоту Google.
	videoDailyLimit = 10000
	// searchPace распределяет страницы поиска по обычным 24 часам.
	searchPace = 24 * time.Hour / searchDailyLimit
)

// projectQuota разделяет поисковые вызовы и запросы к видео. Все изменяемые поля
// защищает mu. Метод допуска не ожидает наступления срока и не удерживает mu во время I/O.
type projectQuota struct {
	// mu сериализует расчёт срока, допуск очередного запроса и учёт отказов Google.
	mu sync.Mutex
	// pacific содержит часовой пояс суточного сброса Google.
	pacific *time.Location
	// resetAt содержит ближайшую полночь дня, в котором учитываются запросы.
	resetAt time.Time
	// searchUsed хранит число начатых запросов страниц в текущих сутках.
	searchUsed int
	// videoUsed хранит число начатых запросов к видео для обнаружения в текущих сутках.
	videoUsed int
	// nextSearch содержит срок допуска нового прохода с учётом всех запрошенных страниц.
	nextSearch time.Time
	// searching удерживает единственный допуск к проходу, включая продолжения после полуночи.
	searching bool
	// searchBlocked и videoBlocked содержат сроки ограничений Google для двух категорий.
	searchBlocked time.Time
	videoBlocked  time.Time
}

// beginSearch резервирует единственный полный проход. Если другая попытка ещё
// выполняется, следующий вызов вернётся в Manager с задержкой вместо ожидания под слотом.
func (q *projectQuota) beginSearch(now time.Time, fallback time.Duration) time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.resetDayLocked(now)
	// delay содержит текущую обязательную паузу между проходами.
	delay := q.delayLocked(now, restSearch)
	if q.searching {
		return max(delay, fallback)
	}
	if delay == 0 {
		q.searching = true
	}
	return delay
}

// finishSearch освобождает допуск при любом исходе зарезервированного прохода.
func (q *projectQuota) finishSearch() {
	q.mu.Lock()
	q.searching = false
	q.mu.Unlock()
}

// resetDayLocked открывает новые сутки квоты в местную полночь даже при смене смещения UTC.
// Подтверждённые сроки от Google могут переходить через границу суток.
func (q *projectQuota) resetDayLocked(now time.Time) {
	if now.Before(q.resetAt) {
		return
	}
	// local задаёт текущую календарную дату Google.
	local := now.In(q.pacific)
	q.resetAt = time.Date(local.Year(), local.Month(), local.Day()+1, 0, 0, 0, 0, q.pacific)
	q.searchUsed = 0
	q.videoUsed = 0
	q.nextSearch = time.Time{}
}

// delayLocked рассчитывает остаток паузы для операции, учитывая срок Google,
// суточную квоту и темп новых поисковых проходов. Вызывается под mu после resetDayLocked.
func (q *projectQuota) delayLocked(now time.Time, operation restOperation) time.Duration {
	// until содержит самый поздний из применимых сроков.
	until := q.videoBlocked
	if operation.search() {
		until = q.searchBlocked
		if q.searchUsed >= searchDailyLimit && q.resetAt.After(until) {
			until = q.resetAt
		}
		if operation == restSearch {
			// paced заканчивается не позднее полуночи, когда начинается новый суточный бюджет.
			paced := q.nextSearch
			if paced.After(q.resetAt) {
				paced = q.resetAt
			}
			if paced.After(until) {
				until = paced
			}
		}
	} else if q.videoUsed >= videoDailyLimit && q.resetAt.After(until) {
		until = q.resetAt
	}
	return max(until.Sub(now), 0)
}

// take учитывает попытку перед отправкой. Продолжение уже допущенного поиска обходит
// только темп первого запроса. Проверка ключа не расходует локальный бюджет обнаружения.
func (q *projectQuota) take(now time.Time, operation restOperation) time.Duration {
	if operation == restKeyProbe {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.resetDayLocked(now)
	// delay равна нулю, если допустимо сразу зарезервировать очередной запрос.
	if delay := q.delayLocked(now, operation); delay > 0 {
		return delay
	}
	if operation.search() {
		q.searchUsed++
		if q.nextSearch.Before(now) {
			q.nextSearch = now
		}
		q.nextSearch = q.nextSearch.Add(searchPace)
	} else {
		q.videoUsed++
	}
	return 0
}

// searchDelay сообщает срок следующего полного прохода, не расходуя очередной запрос.
func (q *projectQuota) searchDelay(now time.Time) time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.resetDayLocked(now)
	return q.delayLocked(now, restSearch)
}

// pause сохраняет подтверждённое общее ограничение для соответствующей категории.
// При daily срок действует как минимум до полуночи по календарю квоты.
func (q *projectQuota) pause(now time.Time, operation restOperation, retry time.Duration, daily bool) time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.resetDayLocked(now)
	// until содержит наиболее поздний применимый срок возобновления запросов.
	until := now.Add(retry)
	if daily && q.resetAt.After(until) {
		until = q.resetAt
	}
	// blocked указывает срок общей паузы категории затронутой операции.
	blocked := &q.videoBlocked
	if operation.search() {
		blocked = &q.searchBlocked
	}
	if until.After(*blocked) {
		*blocked = until
	}
	return blocked.Sub(now)
}
