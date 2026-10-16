# hhmon — мониторинг новых вакансий hh.ru

Ловит **только что опубликованные** вакансии через публичный RSS-поиск hh.ru
(API сейчас отдаёт 403), складывает их в SQLite, подтягивает полное описание
и генерирует сопроводительное письмо выбранной LLM.

Работает циклично: новая подходящая вакансия → письмо в лог и в БД.

## Почему RSS

Официальное API (`api.hh.ru`) сейчас недоступно (403). Но поиск отдаёт RSS,
отсортированный по времени публикации, с точностью до секунды:

```
https://hh.ru/search/vacancy/rss?text=golang&area=1&order_by=publication_time&search_period=1
```

Авторизация для мониторинга **не нужна**.

## Сборка

```bash
go build -o hhmon .
```

## Запуск

```bash
./hhmon -config config.json            # цикличный режим
./hhmon -config config.json -once      # один цикл и выход
./hhmon -config config.json -seed      # только запомнить текущие, без писем
./hhmon -config config.json -limit 3   # обрабатывать не больше 3 новых за цикл
./hhmon -list 10                       # показать 10 последних вакансий с письмами
```

Останов — `Ctrl+C` (graceful shutdown).

## Конфиг

```jsonc
{
  "poll_interval_seconds": 20,   // как часто опрашивать RSS
  "request_jitter_ms": 800,      // случайная пауза между запросами (анти-бан)
  "llm_timeout_seconds": 240,    // таймаут на генерацию одного письма
  "seed_on_first_run": true,     // при пустой БД запомнить текущие, не генерируя письма
  "db_path": "hhmon.db",
  "log_path": "hhmon.log",

  "llm": {                       // LLM по умолчанию для всех поисков
    "provider": "ollama",
    "model": "qwen2.5:14b",
    "base_url": "http://localhost:11434",
    "api_key": "",
    "temperature": 0.6,
    "max_tokens": 1200
  },

  "searches": [                  // каждый поиск независим
    {
      "name": "Go backend",
      "query": "golang",
      "areas": [1, 2],           // 1 = Москва, 2 = СПб
      "search_period_days": 1,
      "include_keywords": [],    // если не пусто — в заголовке должно быть хотя бы одно
      "exclude_keywords": ["1С", "QA", "тестировщик"],
      "profile_paths": ["profile.md", "profiles/go.md"],  // общий профиль + акцент под поиск
      "llm": null                // опционально: свой LLM для этого поиска
    }
  ]
}
```

**Своё инфо под каждый поиск.** Три способа (любой из них):
- `profile_paths` — список файлов, склеиваются по порядку: обычно `["profile.md", "profiles/<акцент>.md"]`;
- `profile_path` — один файл;
- `profile` — текст прямо в конфиге.

В репозитории уже заполнены `profile.md` (общий) и акценты `profiles/go.md`,
`profiles/php.md`, `profiles/highload.md`, `profiles/blockchain.md`, `profiles/ml.md`,
`profiles/data.md`, `profiles/embedded.md`.
Так под Go и под PHP письмо уходит с разными акцентами, но на одних фактах.

## LLM-провайдеры

Слой переключаемый, задаётся в `llm.provider` (можно переопределить на каждый поиск).

| provider    | Ключ | Как получить | Примечание |
|-------------|------|--------------|------------|
| `ollama`    | нет  | локально     | **без лимитов**, нужна своя модель |
| `opencode`  | да   | opencode.ai/zen | есть бесплатные модели |
| `deepseek`  | да   | platform.deepseek.com | `deepseek-chat` |
| `gemini`    | да   | AI Studio    | бесплатный лимит |
| `groq`      | да   | console.groq.com | очень быстрый |
| `openrouter`| да   | openrouter.ai | куча моделей |

Примеры:

```jsonc
// Локально, без лимитов и ключей
{ "provider": "ollama", "model": "qwen2.5:14b", "base_url": "http://localhost:11434" }

// Ollama Cloud: бесплатная быстрая модель (нужен ollama-аккаунт)
{ "provider": "ollama", "model": "gemma4:31b-cloud", "base_url": "http://localhost:11434" }

// OpenCode Zen (бесплатные модели), ключ из opencode.ai/zen
{ "provider": "opencode", "model": "deepseek-v4.1-flash-free", "api_key": "sk-..." }

// DeepSeek
{ "provider": "deepseek", "model": "deepseek-chat", "api_key": "sk-..." }
```

> Важно: часть бесплатных моделей OpenCode Zen **недоступна из РФ**
> (например `cat-jam-free`). Если модель не отвечает — попробуй другую
> (`deepseek-v4.1-flash-free`, `glm-5.3-flash-free`, `mimo-v2.5-free`).

> Важно: бесплатный тариф OpenCode Zen работает **только внутри самого OpenCode**
> (при вызове извне возвращает `FreeTierError`). Для внешнего скрипта нужен
> платный API-ключ opencode, либо используй `ollama`/`groq`/`deepseek`/`gemini`.

> «Безлимит» бывает только у локального Ollama. У всех облачных тарифов
> есть лимиты по запросам.

### Цепочка с автопереключением

`llm_chain` — список провайдеров по приоритету. Если у текущего кончилась
квота, ответил 429/403/402, таймаут или ошибка сети — берётся следующий.
Успешный провайдер запоминается, и следующий запрос стартует уже с него
(лишних запросов к «мёртвой» модели нет).

```jsonc
"llm_chain": [
  { "provider": "deepseek", "model": "deepseek-chat", "api_key": "sk-..." },
  { "provider": "groq", "model": "llama-3.3-70b-versatile", "api_key": "gsk-..." },
  { "provider": "ollama", "model": "gemma4:31b-cloud", "base_url": "http://localhost:11434" },
  { "provider": "ollama", "model": "qwen2.5:14b", "base_url": "http://localhost:11434" }
]
```

`llm_chain` можно задать глобально и/или точечно внутри конкретного поиска
(`searches[].llm_chain`) — он имеет приоритет. Если цепочки нет, используется
одиночный `llm`.

## Как это помогает «залететь первым»

- `order_by=publication_time` + опрос каждые 15–30 сек.
- В лог пишется и ссылка, и готовое письмо — можно быстро откликнуться.
- `seed_on_first_run` не даёт заспамить письмами по старой ленте при первом старте.

## Дисклеймер

Инструмент для личного использования. Массовый автоматический отклик
противоречит правилам hh.ru и может привести к капче/блоку — здесь отклик
не автоматизирован, скрипт только готовит письмо.
