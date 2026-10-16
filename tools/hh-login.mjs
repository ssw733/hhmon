// Логин на hh.ru в постоянном профиле Chrome (Playwright).
// Профиль сохраняется, поэтому отклики (tools/hh-apply.mjs) используют тот же
// вход и повторный логин не нужен.
//
// Установка: npm i playwright && npx playwright install chromium  (если нет Google Chrome)
// Запуск:    node tools/hh-login.mjs [--dir ~/.hhmon-chrome]

import { chromium } from 'playwright';
import os from 'node:os';
import path from 'node:path';

function arg(name, def) {
  const i = process.argv.indexOf(name);
  return i >= 0 && i + 1 < process.argv.length ? process.argv[i + 1] : def;
}

const dir = arg('--dir', process.env.HHMON_CHROME_DIR || path.join(os.homedir(), '.hhmon-chrome'));
const stealthArgs = ['--disable-blink-features=AutomationControlled', '--start-maximized'];

let ctx;
try {
  ctx = await chromium.launchPersistentContext(dir, {
    headless: false, channel: 'chrome', args: stealthArgs,
    locale: 'ru-RU', timezoneId: 'Europe/Moscow', viewport: null,
  });
} catch (e) {
  console.warn('Chrome не запустился (' + e.message.split('\n')[0] + '), беру встроенный Chromium');
  ctx = await chromium.launchPersistentContext(dir, {
    headless: false, args: stealthArgs, locale: 'ru-RU', viewport: null,
  });
}
await ctx.addInitScript(() => Object.defineProperty(navigator, 'webdriver', { get: () => undefined }));
const page = ctx.pages()[0] ?? (await ctx.newPage());

let opened = false;
for (let a = 1; a <= 3 && !opened; a++) {
  try {
    await page.goto('https://hh.ru/account/login', { waitUntil: 'domcontentloaded', timeout: 60000 });
    opened = true;
  } catch (e) {
    console.error(`Попытка ${a}: ${e.message.split('\n')[0]}`);
    await page.waitForTimeout(3000);
  }
}
if (!opened) {
  console.error('✗ hh.ru не открылся. Проверь сеть/VPN.');
  await page.waitForTimeout(5 * 60 * 1000);
  await ctx.close();
  process.exit(1);
}

console.log('→ Войди в аккаунт hh.ru (пароль/код). Профиль: ' + dir);
console.log('→ Как войдёшь — скрипт сам завершится и запомнит сессию.');

const deadline = Date.now() + 5 * 60 * 1000;
let loggedIn = false;
while (Date.now() < deadline) {
  const url = page.url();
  const cookies = await ctx.cookies('https://hh.ru');
  loggedIn =
    !url.includes('/account/login') ||
    cookies.some((c) => ['hhuid', 'hhrole', 'crypted_id', 'hh_sess_uid'].includes(c.name));
  if (loggedIn) break;
  await page.waitForTimeout(2000);
}

if (loggedIn) {
  console.log('✓ Сессия сохранена в профиле: ' + dir);
} else {
  console.error('✗ Не дождался входа (таймаут).');
  process.exitCode = 1;
}
await ctx.close();
