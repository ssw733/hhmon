// Отклик на вакансию hh.ru через постоянный профиль Chrome (Playwright).
// Использует тот же профиль, что и tools/hh-login.mjs → залогинен без session.json.
//
// Запуск:
//   node tools/hh-apply.mjs --id 138565634 --letter /tmp/letter.txt [--dir ~/.hhmon-chrome] [--dry]
// Печатает JSON: {"status":"submitted"|"dry_run"|"failed", "error":"..."}

import { chromium } from 'playwright';
import { readFileSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';

function arg(name, def) {
  const i = process.argv.indexOf(name);
  return i >= 0 && i + 1 < process.argv.length ? process.argv[i + 1] : def;
}
function out(obj) {
  console.log(JSON.stringify(obj));
}

const id = arg('--id');
const letterFile = arg('--letter');
const dir = arg('--dir', process.env.HHMON_CHROME_DIR || path.join(os.homedir(), '.hhmon-chrome'));
const dry = process.argv.includes('--dry');

if (!id || !letterFile) {
  out({ status: 'failed', error: 'нужны --id и --letter' });
  process.exit(0);
}

let letter = '';
try {
  letter = readFileSync(letterFile, 'utf8');
} catch (e) {
  out({ status: 'failed', error: 'не читается письмо: ' + e.message });
  process.exit(0);
}

const stealthArgs = ['--disable-blink-features=AutomationControlled', '--start-maximized'];
let ctx;
try {
  ctx = await chromium.launchPersistentContext(dir, {
    headless: false, channel: 'chrome', args: stealthArgs,
    locale: 'ru-RU', timezoneId: 'Europe/Moscow', viewport: null,
  });
} catch {
  ctx = await chromium.launchPersistentContext(dir, {
    headless: false, args: stealthArgs, locale: 'ru-RU', viewport: null,
  });
}
await ctx.addInitScript(() => Object.defineProperty(navigator, 'webdriver', { get: () => undefined }));
const page = ctx.pages()[0] ?? (await ctx.newPage());

let status = 'failed';
let error = '';

try {
  await page.goto(`https://hh.ru/vacancy/${id}`, { waitUntil: 'domcontentloaded', timeout: 60000 });

  if (page.url().includes('/account/login')) {
    error = 'сессия не авторизована — запусти node tools/hh-login.mjs';
    throw new Error(error);
  }

  const respond = page
    .locator('[data-qa="vacancy-response-link-top"], a:has-text("Откликнуться"), button:has-text("Откликнуться")')
    .first();
  await respond.click({ timeout: 20000 });
  await page.waitForTimeout(1500);

  const letterBox = page
    .locator('[data-qa="vacancy-response-popup-form-letter-input"], textarea[name="letter"]')
    .first();
  await letterBox.waitFor({ state: 'visible', timeout: 20000 });
  await letterBox.fill(letter);

  if (dry) {
    out({ status: 'dry_run' });
    await ctx.close();
    process.exit(0);
  }

  const submit = page
    .locator('[data-qa="vacancy-response-submit-popup"], button:has-text("Отправить"), button:has-text("Откликнуться")')
    .first();
  await submit.click({ timeout: 20000 });
  await page.waitForTimeout(4000);

  const body = (await page.locator('body').innerText().catch(() => '')).slice(0, 8000);
  if (/что-то пошло не так|попробуйте позже|заполните обязательн|обязательн/i.test(body)) {
    error = 'hh вернул сообщение об ошибке/обязательных полях';
    throw new Error(error);
  }
  status = 'submitted';
} catch (e) {
  if (!error) error = String(e).split('\n')[0];
  status = 'failed';
} finally {
  out(status === 'submitted' ? { status } : { status, error });
  await page.waitForTimeout(1000).catch(() => {});
  await ctx.close().catch(() => {});
}
