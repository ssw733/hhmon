// Локальный логин на hh.ru через браузер (Playwright).
// Пароль и коды подтверждения вводятся только в браузере — никуда не передаются.
//
// Установка (один раз):
//   npm i playwright
//   npx playwright install chromium
//
// Запуск:
//   node tools/hh-login.mjs [session.json]

import { chromium } from 'playwright';

const out = process.argv[2] || 'session.json';

const browser = await chromium.launch({ headless: false });
const context = await browser.newContext({ locale: 'ru-RU' });
const page = await context.newPage();

await page.goto('https://hh.ru/account/login', { waitUntil: 'domcontentloaded' });
console.log('→ Войди в аккаунт hh.ru в открывшемся окне (телефон/почта, код подтверждения).');
console.log('→ Как войдёшь — сессия сохранится автоматически. Ждём до 5 минут...');

const deadline = Date.now() + 5 * 60 * 1000;
let ok = false;
while (Date.now() < deadline) {
  const url = page.url();
  const cookies = await context.cookies('https://hh.ru');
  const loggedIn =
    !url.includes('/account/login') ||
    cookies.some((c) => ['hhuid', 'hhrole', 'crypted_id', 'hh_sess_uid'].includes(c.name));
  if (loggedIn) {
    ok = true;
    break;
  }
  await page.waitForTimeout(2000);
}

if (ok) {
  const state = await context.storageState({ path: out });
  state.userAgent = await page.evaluate(() => navigator.userAgent);
  const { writeFileSync } = await import('node:fs');
  writeFileSync(out, JSON.stringify(state, null, 2));
  console.log('✓ Сессия сохранена: ' + out);
} else {
  console.error('✗ Не дождался входа (таймаут). Попробуй снова.');
  process.exitCode = 1;
}
await browser.close();
