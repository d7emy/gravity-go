# gravity-go

**[English](README.en.md)**

> اشتراك OpenCode حقك ضرب حد الاستخدام بنص الريفاكتور. Antigravity ما ضرب. هذا هو الجسر.

موديلات Antigravity كـ API محلي متوافق مع Anthropic وOpenAI. خدمة Go في **ملف تنفيذي واحد** — بدون Node وبدون CDN وبدون ملفات إضافية.

## الأزمة اللي يحلها

تدفع لـ OpenCode، وبعدين:

- جلسة طويلة **تترفض من الأساس** (صارت فعلًا لجلسة 669k توكن)،
- إعداد مليان أدوات يصطدم بسقف **100 أداة**،
- ريفاكتور يموت على **429** وباقي لك ثلاث ملفات،
- والعداد يحسب وأنت تطالع `retry-after`.

الحل: وجّه OpenCode إلى `http://127.0.0.1:8964` واستهلك **الحصة الأسبوعية لـ Antigravity** بدل الدفع بالتوكن — Gemini وClaude بنفس حلقة الوكيل. وإذا حساب Google خلصت حصته، الطلب **يتحول تلقائيًا للحساب التالي** داخل نفس الطلب. تشوف رد ناجح، مو خطأ.

## ليش يهم مستخدمي OpenCode

- **يتكلم البروتوكولين:** `POST /v1/messages` لعملاء Anthropic و`POST /v1/chat/completions` لعملاء OpenAI — بث، أدوات، صور، Thinking Blocks.
- **مصمم للجلسات الطويلة:** 100,000 رسالة و10,000 أداة للطلب الواحد، لأن كل استدعاء أداة ونتيجته رسالة.
- **ينجو من نفاد الحصة:** حساب واحد يخدم الكل لين تقول Google ‏`429 quota_exhausted` — بعدها تهدئة + تأخير الحساب لآخر الطابور + إعادة محاولة شفافة، والترتيب محفوظ حتى بعد إعادة التشغيل.
- **ملف واحد:** اللوحة مضمنة في الملف (`go:embed`) وتشتغل بدون إنترنت.

أسرع ربط (نفس نمط `baseURL` في [توثيق OpenCode](https://opencode.ai/docs/providers/)):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "anthropic": {
      "options": { "baseURL": "http://127.0.0.1:8964" }
    }
  }
}
```

أو كمزود مستقل بنمط OpenAI (مثل أمثلة Ollama في نفس التوثيق):

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": {
    "gravity-go": {
      "npm": "@ai-sdk/openai-compatible",
      "name": "gravity-go (local)",
      "options": { "baseURL": "http://127.0.0.1:8964/v1" },
      "models": {
        "gemini-3.8-flash-high": { "name": "Gemini 3.8 Flash (High)" }
      }
    }
  }
}
```

المفتاح صوري (`ANTHROPIC_API_KEY=dummy`) — البروكسي على loopback وما يحتاج مفتاح.

## التشغيل

يحتاج **Go 1.25.7+** فقط.

```bash
git clone https://github.com/<you>/gravity-go.git
cd gravity-go
go run .                    # تشغيل مباشر — نفسها start
go build -o gravity-go.exe .
```

يشتغل على المنفذ **8964** واللوحة على http://localhost:8964/quota

```bash
gravity-go start [-p PORT] [-v]  # تشغيل البروكسي (الافتراضي 8964)
gravity-go login                 # إضافة حساب Google عبر OAuth
gravity-go accounts              # عرض الحسابات
gravity-go logout-ide            # تسجيل خروج IDE ومسح جلسته
gravity-go version               # عرض النسخة
```

## الدخول: مساران بالترتيب

1. **OAuth** — `gravity-go login` يفتح موافقة Google ويحفظ التوكن في `~/.gravity-go/auth.json`. ضف عدة حسابات وهي تتناوب تلقائيًا.
2. **IDE المحلي** — ما فيه OAuth؟ التوكن يُقرأ مباشرة من `state.vscdb` حق Antigravity (قراءة فقط).

البيانات في `~/.gravity-go` — ولو ما وُجد وكان فيه `~/.anti-api` قديم، يُستخدم كما هو بدون ترحيل. تجاوز الكل عبر `GRAVITY_DATA_DIR`.

## وش تحصل

| الموديل | ملاحظة |
| --- | --- |
| `gemini-3.8-flash-high` | الأحدث، الاختيار الافتراضي |
| `gemini-3.7-flash-high` | السابق، ما زال موجود |
| `gemini-3.1-pro-high` | أقوى، Thinking Blocks طويلة |
| `claude-opus-4-6-thinking` | حصة أسبوعية منفصلة عن Gemini |

القائمة المعروضة في `GET /v1/models` تصفية فقط — موديلات أكثر قابلة للاستدعاء بالاسم. الأسماء تُترجم داخليًا (`gemini-3.8-flash-high` ← `gemini-3.8-flash-tiered`).

- **Anthropic:** `POST /v1/messages` (و`/v1beta/messages` و`/messages`).
- **OpenAI:** `POST /v1/chat/completions` — مع `reasoning_effort` و`stream_options.include_usage`.
- **بحث:** `GET|POST /search` — بحث ويب مدعوم، يرجع إجابة + مصادر.
- **لوحة:** `/quota` و`/quota/json` و`/usage` و`/settings` و`/logs` و`/auth/*` و`/accounts/*` — بدون CDN وتشتغل أوفلاين.

المرجع الكامل: [API.md](API.md) للمختصر، و[LOCALAPI.md](LOCALAPI.md) لكل مسار وحقل.

## التدوير والإيقاف

- الحسابات **واحدًا واحدًا بترتيب التخزين** — الأول في `accounts.json` يُستنزف أولًا. الترتيب مقصود أنه غير واعٍ بالحصة لأن أرقام الحصة المخزنة غير دقيقة لكل طلب.
- زر الإيقاف في اللوحة (أو `POST /accounts/{id}/enabled`) يجمّد الحساب بدون حذف بياناته، والرجوع عنه يمسح التهدئة القديمة.
- أوقف **الكل** والبروكسي يرد `503 All accounts are paused` بدل ما يخدم من وراك.

## الإعدادات

| المتغير | الافتراضي | الغرض |
| --- | --- | --- |
| `GRAVITY_DATA_DIR` | `~/.gravity-go` | البيانات والإعدادات |
| `GRAVITY_HOST` / `GRAVITY_PORT` | `127.0.0.1` / `8964` | العنوان والمنفذ (`-p` يغلب) |
| `GRAVITY_ACCOUNT_CONCURRENCY` | `1` | طلبات متوازية لكل حساب (1–8) |
| `GRAVITY_ACCOUNT_INTERVAL_MS` | `1000` | أقل فاصل بين طلبين لنفس الحساب |
| `GRAVITY_MIN_REQUEST_INTERVAL_MS` | `250` | الفاصل العام |
| `GRAVITY_SEARCH_TOKEN` | فارغ | توكن إجباري على `/search` |
| `GRAVITY_NO_OPEN` / `GRAVITY_OAUTH_NO_OPEN` | فارغ | `1` يمنع فتح المتصفح |
| `GRAVITY_INSECURE_TLS` | فارغ | `1` يعطل التحقق من الشهادات (للبروكسيات الشركية فقط) |
| `GRAVITY_JITTER` | مفعّل | `0` يعطل العشوائية للاختبارات الحتمية |

> ⚠️ تحذير لطيف بأسنان: `GRAVITY_HOST=0.0.0.0` يفتح البروكسي — اللي ما يحتاج مصادقة وشايل توكنات Google — على كل الشبكة المحلية. إذا ما كنت ناوي تعزم القهوة على حصتك، راجع متغيرات البيئة.

## تنبيهات صادقة (مقاسة، مو تخمين)

- **معاملات العشوائية ما تشتغل:** `temperature` و`top_p` و`top_k` توصل للسلك (مثبت باختبار) **والاتجاه upstream يتجاهلها**. `temperature: 0` رجعت أربع صياغات مختلفة لأربع طلبات. لا تعتمد عليها للتكرارية.
- **`stop_reason` يكذب عند القطع:** يرجع `end_turn` حتى مع تجاوز `max_tokens`. قارن `usage.output_tokens` بالـ `max_tokens` المطلوب.
- **الصور لازم base64:** الروابط تُستبدل بعلامة `[image omitted: …]` واضحة بدل ما يهلوس الموديل عن صورة ما شافها.
- **أسعار لوحة الاستهلاك تمثيل:** الفوترة حصة أسبوعية مو بالتوكن، فكل أرقام الدولار تقديرات.

## وش مو موجود (عمدًا)

مزود antigravity فقط — لا codex ولا copilot ولا routing ولا أنفاق (هات ngrok بنفسك) ولا تحديث ذاتي (`GET /updates/check` يقولها بأدب) ولا embeddings — المرفوض منها يرجع `501` صريح.

## الفحص

```bash
go run ./cmd/check         # تنسيق + vet + بناء + اختبارات + كاشف التسابق
go test ./...              # الاختبارات فقط
```

كاشف التسابق يحتاج مترجم C (cgo) — غيابه مشكلة جهازك مو مشكلة الكود:
`winget install -e --id BrechtSanders.WinLibs.POSIX.UCRT.Base`

## الخرائط

- [README.en.md](README.en.md) — النسخة الإنجليزية.
- [API.md](API.md) — مرجع التكامل السريع.
- [LOCALAPI.md](LOCALAPI.md) — العقد الكامل لكل مسار.
- [DESIGN.md](DESIGN.md) — توكنات تصميم اللوحة، لل CSS فقط.

---

*مصمم للحظة اللي تقول فيها خطتك المدفوعة "هدّي السرعة" وموعد التسليم يقول "ههه لا". إذا أنقذت gravity-go نشرة لك، حط نجمة عشان اللي بعده غرقان في 429 يلقى قارب النجاة أسرع.*
