# Администрирование Remote Claude

Документ описывает установку нового автономного сервера. Никакие адреса, учётные записи, ключи или настройки конкретной инфраструктуры не встроены в клиент: адрес SSH попадает в приглашение администратора, а публичный fingerprint вычисляется на сервере.

## 1. Архитектура и границы доверия

На сервере в `/opt/remote-claude` находятся:

- `remote-claude` — серверный бинарник и обработчики enrollment, SFTP, shell, Git и server jobs;
- `claude` — закреплённая версия Claude Code;
- `rg` — необязательный серверный ripgrep для fallback-поиска;
- `shell` и `windows/pwsh` — адаптеры команд;
- `auth-sync.py` и systemd units — центральное обновление общей авторизации;
- `add-account.py`, `configure-resources.py`, `pool-governor.py` — административные helper-скрипты;
- `clients/` — манифест и файлы автоматического обновления клиентов.

Каждый пользователь получает отдельный Linux-аккаунт с домашним каталогом, SSH-ключами, историей Claude и серверными checkout. Все управляемые аккаунты могут использовать одну подписку Claude. **Отдельные аккаунты не увеличивают лимиты подписки.**

Администратор root полностью входит в модель доверия. Пользователь аккаунта может прочитать собственную серверную копию OAuth-авторизации, поэтому доступ выдаётся только доверенным людям. На ПК пользователей OAuth-файлы не копируются.

Использование общей подписки несколькими людьми должно соответствовать правилам провайдера. Проект не предназначен для обхода ограничений и не гарантирует отсутствие блокировок.

## 2. Требования

Поддерживаемая базовая конфигурация:

- Ubuntu Linux с systemd и OpenSSH server;
- SSH host key Ed25519: `/etc/ssh/ssh_host_ed25519_key.pub`;
- Python 3;
- `sshfs` и FUSE для локального режима (живой вид игнорируемых файлов);
- tmux — для серверного режима; Docker и sudo — только для задач проектов с `.remote-claude/Dockerfile`;
- `gh` — если задачи должны создавать pull request;
- Go toolchain из `go.mod` либо Docker для сборки;
- установленный Claude Code **2.1.292**.

Пример базовых пакетов:

```sh
sudo apt-get update
sudo apt-get install -y openssh-server sshfs python3 git curl ca-certificates
```

Для серверного режима дополнительно:

```sh
sudo apt-get install -y docker.io tmux sudo
```

Установите `gh` по официальной инструкции GitHub. Установите Claude Code по официальной инструкции Anthropic и проверьте точную версию:

```sh
/absolute/path/to/claude --version
# Ожидается: 2.1.292 (Claude Code)
```

Не разрешайте автоматическое обновление закреплённого Claude до проверки адаптера на новой версии.

## 3. Сборка и установка сервера

Скопируйте репозиторий на сервер и переключитесь на релизный тег. Обычная сборка без `--dev` проверяет чистоту дерева и соответствие тега версии:

```sh
git checkout v0.2.0
python3 scripts/build-release.py
python3 scripts/check-release.py
```

Команда создаёт шесть клиентских архивов и серверный update tree `dist/clients/`. Для сервера Linux amd64 бинарник по умолчанию берётся из `dist/clients/0.2.0/linux-amd64/remote-claude`; для другой архитектуры укажите `--server-binary`.

Установка:

```sh
sudo python3 -I scripts/install-server.py \
  --claude-binary /absolute/path/to/claude \
  --ripgrep /absolute/path/to/rg
```

`--ripgrep` необязателен: без него локальный поиск продолжит работать, а неподдерживаемый fallback будет зависеть от другой доступной конфигурации. Для повторяемой установки рекомендуется передать `dist/clients/0.2.0/linux-amd64/remote-claude-rg`.

Скрипт:

- проверяет Ubuntu, root и версию Claude;
- атомарно заменяет бинарники;
- устанавливает helper-скрипты, Dockerfile и systemd units;
- создаёт shell/PowerShell shims;
- включает `remote-claude-auth-sync.timer`;
- **не создаёт пользователей и не копирует авторизацию root**.

Проверка:

```sh
/opt/remote-claude/remote-claude version
systemctl status remote-claude-auth-sync.timer
```

До первого входа под управляемым аккаунтом auth-sync будет сообщать об отсутствии пригодной авторизации — это ожидаемо.

## 4. Первый аккаунт и Claude login

Создайте пользователя, подставив публичный SSH hostname или адрес вашего сервера:

```sh
sudo python3 -I /opt/remote-claude/add-account.py \
  --host ssh.example.com developer
```

Скрипт:

1. создаёт Linux-аккаунт и управляемый маркер `.remote-claude-managed`;
2. создаёт каталоги с ограниченными правами;
3. генерирует одноразовую bootstrap-пару Ed25519;
4. разрешает bootstrap-ключу только команду `remote-claude enroll`;
5. вычисляет fingerprint публичного SSH host key;
6. сохраняет приглашение `/root/remote-claude-invitations/remote-claude-developer.json` с правами `0600`;
7. удаляет локальную копию закрытого bootstrap-ключа после помещения в приглашение.

Имя может быть обычным безопасным Linux username, а не только `claude-devN`. По умолчанию UID выбирает `useradd`.

### Первый вход в Claude

Войдите под управляемым пользователем, не под root:

```sh
sudo -iu developer
DISABLE_AUTOUPDATER=1 /opt/remote-claude/claude
```

Выполните `/login`, проверьте правильность аккаунта и выйдите. Затем от администратора:

```sh
sudo systemctl start remote-claude-auth-sync.service
sudo systemctl status remote-claude-auth-sync.service
journalctl -u remote-claude-auth-sync.service -o cat
```

Сервис должен обнаружить авторизацию. Он не печатает access/refresh tokens: в журнал попадают только короткие SHA-256 fingerprints токенов.

Передайте приглашение пользователю по защищённому каналу. Не копируйте его в репозиторий, Release, общий каталог или тикет. После успешного `setup` оно больше не нужно.

## 5. Дополнительные аккаунты

### С администраторского ПК

Когда хотя бы один управляемый аккаунт уже авторизован:

```sh
python3 scripts/new-invitation.py \
  --host ssh.example.com seconddeveloper
```

Скрипт по SSH устанавливает актуальные helper-скрипты, создаёт аккаунт, запускает auth-sync и скачивает приглашение в локальный `private/remote-claude-seconddeveloper.json` с правами `0600`. Каталог `private/` исключён из Git.

Параметры:

- `--admin-user NAME` — SSH-администратор вместо root; нужен неинтерактивный `sudo`;
- `--proxy`, `--proxy-host`, `--proxy-port` — HTTP CONNECT-прокси для SSH;
- `--resources` — дополнительно настроить resource limits и server mode; без этого флага Docker/tmux не требуются;
- `--resume` — после сбоя повторно запустить auth-sync и скачать исходное приглашение, не меняя bootstrap-ключ.

В `--host` можно указать порт: `ssh.example.com:2222`. IPv6 записывается как `2001:db8::1`, а IPv6 с портом — как `[2001:db8::1]:2222`. Вместо `--host` можно задать `REMOTE_CLAUDE_HOST`.

Если аккаунт уже создан, а auth-sync или скачивание приглашения завершились ошибкой, повторите ту же команду с `--resume`. Восстановление разрешено только для управляемого аккаунта с сохранённым `enrollment.pending` и root-owned приглашением; частично созданный или уже зарегистрированный аккаунт скрипт не принимает.

### Непосредственно на сервере

```sh
sudo python3 -I /opt/remote-claude/add-account.py \
  --host ssh.example.com seconddeveloper
sudo systemctl start remote-claude-auth-sync.service
```

Auth-sync находит аккаунты не по имени, а по `.remote-claude-managed`. Не создавайте этот маркер вручную у обычных пользователей: иначе сервис будет рассматривать их как получателей общей авторизации.

## 6. Регистрация клиента и отзыв доступа

При `setup` клиент создаёт собственный Ed25519-ключ и отправляет только публичную часть. Сервер однократно заменяет bootstrap-запись в `authorized_keys` ключом устройства. После enrollment разрешены SSH-подключения пользователя с опциями `no-agent-forwarding,no-X11-forwarding`; это не restricted-shell.

Для отзыва доступа:

1. Удалите публичный ключ устройства из `/home/USER/.ssh/authorized_keys`.
2. Завершите активные SSH/tmux/Claude-процессы при необходимости.
3. Заблокируйте Linux-аккаунт, если доступ нужно прекратить полностью.
4. Отзовите токены GitHub/Git-хостинга, выданные этому аккаунту.
5. Удалите серверные checkout и домашний каталог только после резервного копирования нужной истории.

Отзыв одного SSH-ключа не отзывает общую Claude-сессию на других аккаунтах. Если сама авторизация скомпрометирована, завершите/отзовите её у провайдера и выполните новый `/login` под управляемым аккаунтом.

Неиспользованное приглашение следует считать действующим bootstrap-доступом: удалите его запись из `authorized_keys`, удалите файл приглашения и выпустите новое, если файл мог быть раскрыт.

## 7. Центральная синхронизация авторизации

`remote-claude-auth-sync.timer` запускает oneshot-сервис примерно раз в минуту.

Алгоритм:

- читает только домашние каталоги с управляемым маркером, без следования symlink для credential-файлов;
- выбирает самую свежую пригодную авторизацию по `expiresAt`;
- примерно за два часа до истечения обновляет её раньше собственного порога Claude;
- выполняет refresh-запрос из child-процесса с UID/GID исходного аккаунта, чтобы применялись его egress-правила;
- заменяет только ключ `claudeAiOauth`, сохраняя остальные данные `.credentials.json`;
- атомарно распространяет обновление на остальные управляемые аккаунты;
- не повторяет отклонённый `invalid_grant` refresh token; сетевые ошибки повторяет не чаще чем через пять минут.

Состояние хранится в `/var/lib/remote-claude-auth/` с правами `0700`. Root-сервис является брокером файлов, но не создаёт отдельную постоянную root-авторизацию Claude.

Проверка логики без сети:

```sh
python3 -I scripts/test_auth_sync.py
```

Если журнал требует повторный вход, выполните `/login` под любым управляемым аккаунтом и снова запустите сервис.

## 8. Единый сетевой выход

Claude выполняется на сервере, поэтому его исходный адрес определяется маршрутизацией сервера. Это уменьшает смену сетевого окружения между устройствами. Дополнительно администратор может направить managed UID через VPN/WireGuard или отдельный egress gateway.

Если используется маршрутизация по UID, задайте диапазон при создании аккаунта:

```sh
sudo python3 -I /opt/remote-claude/add-account.py \
  --host ssh.example.com --uid-min 2001 --uid-max 2099 developer
```

Обе границы указываются вместе. Диапазон должен совпадать с вашими правилами маршрутизации.

**VPN/killswitch не входит в этот репозиторий.** Самостоятельно настройте:

- маршрут managed UID только через нужный интерфейс;
- запрет прямого fallback при остановленном туннеле;
- политику IPv4/IPv6 и DNS;
- восстановление правил после перезагрузки;
- тесты утечки для каждого managed UID.

Проверяйте внешний адрес и невозможность прямого выхода отдельно. Не показывайте публично реальные адреса, fingerprint'ы, правила firewall и список аккаунтов без необходимости.

## 9. Серверный режим и ресурсы

### Включение

Установите tmux. По умолчанию задача работает под аккаунтом разработчика, как вход по SSH, и Docker ей не нужен. Если проекты используют `.remote-claude/Dockerfile`, установите также Docker и sudo и создайте sudoers-файл **до** добавления аккаунтов, которым нужны такие задачи:

```sh
sudo install -m 0440 /dev/null /etc/sudoers.d/remote-claude-jobs
```

`add-account.py` добавит только три root-команды и проверит кандидат через `visudo`:

- `server job-image *`;
- `server job-container *`;
- `server job-list`.

Затем:

```sh
sudo python3 -I /opt/remote-claude/configure-resources.py developer
sudo visudo -cf /etc/sudoers.d/remote-claude-jobs
```

Без аргументов скрипт обрабатывает все аккаунты с управляемым маркером.

Скрипт создаёт:

- пользовательский `remote-claude.slice` для новых локальных сеансов;
- системный `remoteclaude.slice` для общего пула server jobs;
- `remote-claude-jobs@UID.service` для tmux;
- `remote-claude-pool-governor.service` для динамического управления памятью;
- маркер `resources.enabled`;
- базовый Docker image `remote-claude-job-base:latest`.

### Фактические лимиты

Новые локальные сеансы одного аккаунта совместно получают:

- `MemoryHigh=1536M`;
- `MemoryMax=2048M`;
- `CPUQuota=150%`;
- `TasksMax=256`.

Общий пул серверных задач:

- `CPUWeight=10`, `IOWeight=10` — приоритет при конкуренции, не жёсткая CPU-квота;
- `MemoryMax=50%`;
- `TasksMax=4096`;
- до двух tmux-задач на аккаунт;
- до восьми субагентов в задаче без вложенного размножения;
- PID limit контейнера — 1024.

Governor каждые пять секунд пересчитывает мягкий `memory.high`, оставляя резерв памяти серверу. При нехватке памяти он может остановить контейнеры самого тяжёлого аккаунта и перезапустить его job service. История разговора и checkout при этом сохраняются.

Изменения применяются к новым процессам; уже запущенные группы не переносятся. Прямые неуправляемые процессы Claude и root обходят эти бюджеты. Место на диске под `~/server-jobs/` и под репликами локального режима (`~/.local/share/remote-claude/projects/<id>/worktree`: копия всех неигнорируемых файлов проекта) пулом не ограничивается.

### Изоляция, которую не следует переоценивать

Задача без Dockerfile имеет ровно права своего аккаунта: она видит сервер, его сервисы и базы данных так же, как этот пользователь по SSH, и работает автономно без подтверждений (запрещены только слияние PR, force-push и удаление веток). Выдавайте серверный режим тем, кому доверили бы такой вход.

Контейнер получает UID/GID пользователя, dropped capabilities, `no-new-privileges`, host network и домашний каталог пользователя. Docker socket не монтируется. При этом Dockerfile проекта выполняет build steps от root в build container, а домашний каталог доступен задаче.

Это изоляция toolchain и ресурсов для доверенной команды, не sandbox для вредоносного пользователя. Branch protection Git-хостинга важнее permission rules Claude.

## 10. Обновление

Для каждого изменения публикуется новая версия. Пересборка под старой версией не обновит клиентов.

На чистом тегированном checkout:

```sh
go test -race ./...
go vet ./...
python3 -m unittest discover -s scripts -p 'test_*.py'
python3 scripts/build-release.py
python3 scripts/check-release.py
```

GitHub Actions выпускает шесть архивов и `SHA256SUMS` из точного тега. Затем обновите сервер:

```sh
sudo python3 -I scripts/install-server.py \
  --claude-binary /absolute/path/to/claude \
  --ripgrep /absolute/path/to/rg

sudo install -d -m 0755 /opt/remote-claude/clients
sudo cp -a dist/clients/. /opt/remote-claude/clients/
sudo chown -R root:root /opt/remote-claude/clients
sudo find /opt/remote-claude/clients -type d -exec chmod 0755 {} +
sudo find /opt/remote-claude/clients -type f -exec chmod 0644 {} +
```

Не публикуйте development manifest. Клиент отклоняет `development=true`, но серверный каталог всё равно должен заполняться только из проверенного release build.

Клиенты получают новую версию по pinned SSH и проверяют SHA-256. Ошибка скачивания не мешает продолжить работу прежней версии. Перед одновременным обновлением Claude и адаптера отдельно проверьте hooks, shell, MCP, разрешения и reconnect.

Подробности выпуска — [RELEASING.md](RELEASING.md).

## 11. Диагностика и эксплуатация

```sh
systemctl status remote-claude-auth-sync.timer
journalctl -u remote-claude-auth-sync.service -o cat
findmnt -t fuse.sshfs
ps -C claude -o user,pid,rss,%cpu
systemd-cgtop remoteclaude.slice
journalctl -u remote-claude-pool-governor.service
docker ps
```

Состояние локальных проектов находится в `~/.local/share/remote-claude/`. Активный проект имеет lock; stale mount и недоступный bridge очищаются при повторном подключении. Не удаляйте файлы активной сессии вручную.

Регулярно контролируйте:

- свободное место и старые Docker images;
- `~/server-jobs/` с незакоммиченными изменениями;
- срок действия авторизации и ошибки refresh;
- неиспользованные приглашения и authorized_keys;
- обновления ОС, OpenSSH, Docker и зависимостей Go;
- соответствие версии Claude закреплённой.

Не храните в репозитории приглашения, `.credentials.json`, SSH-ключи, реальные production runbooks и отчёты с адресами/именами проектов. Такие материалы принадлежат отдельному защищённому хранилищу.

## 12. Отключение

1. Предупредите пользователей и завершите активные задачи.
2. Остановите `remote-claude-auth-sync.timer` и pool governor.
3. Размонтируйте оставшиеся SSHFS.
4. Отзовите authorized_keys и заблокируйте аккаунты при необходимости.
5. Остановите и удалите server jobs/containers.
6. Удалите systemd units и `/opt/remote-claude` после проверки резервных копий.
7. Домашние каталоги и историю удаляйте отдельным осознанным решением.

Удаление серверной интеграции не затрагивает локальные исходники на ПК. Серверные checkout, напротив, существуют только на сервере — сначала отправьте нужные ветки или сделайте резервную копию.
