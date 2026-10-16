// Локальный логин на hh.ru через браузер (Playwright).
// Пароль и коды подтверждения вводятся только в браузере — никуда не передаются.
//
// Установка (один раз):
//   npm i playwright
//   npx playwright install chromium        # если нет системного Google Chrome
//
// Запуск:
//   node tools/hh-login.mjs [session.json]

import { chromium } from 'playwright';
import { writeFileSync } from 'node:fs';

const out = process.argv[2] || 'session.json';

const stealthArgs = [
  '--disable-blink-features=AutomationControlled',
  '--disable-features=IsolateOrigins,site-per-process',
  '--start-maximized',
];

async function launch() {
  // Сначала пробуем системный Google Chrome — hh режет встроенный Chromium.
  try {
    const b = await chromium.launch({ headless: false, channel: 'chrome', args: stealthArgs });
    console.log('→ Запущен системный Google Chrome');
    return b;
  } catch (e) {
    console.warn('→ Chrome не запустился (' + e.message.split('\n')[0] + '), использую встроенный Chromium');
    return await chromium.launch({ headless: false, args: stealthArgs });
  }
}

const browser = await launch();
const context = await browser.newContext({
  locale: 'ru-RU',
  timezoneId: 'Europe/Moscow',
  viewport: null,
});
const page = await context.newPage();

const UA =
  'Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36';
await context.addInitScript(() => {
  Object.defineProperty(navigator, 'webdriver', { get: () => undefined });
});

let opened = false;
for (let attempt = 1; attempt <= 3 && !opened; attempt++) {
  try {
    await page.goto('https://hh.ru/account/login', { waitUntil: 'domcontentloaded', timeout: 60000 });
    opened = true;
  } catch (e) {
    console.error(`Попытка ${attempt} открыть hh.ru не удалась: ${e.message.split('\n')[0]}`);
    await page.waitForTimeout(3000);
  }
}

if (!opened) {
  console.error('\n✗ hh.ru не открылся. Что проверить:');
  console.error('  • открывается ли hh.ru в твоём обычном браузере (если нет — сеть/VPN);');
  console.error('  • если обычный Chrome открывает, а этот нет — закрой VPN/прокси или наоборот включи;');
  console.error('  • возможно временная блокировка после нагрузки — подожди 10–30 мин.');
  console.error('Окно оставлю открытым на 5 минут для диагностики...');
  await page.waitForTimeout(5 * 60 * 1000);
  await browser.close();
  process.exit(1);
}

console.log('→ Войди в аккаунт hh.ru в открывшемся окне (телефон/почта, код подтверждения).');
console.log('→ Как войдёшь — сессия сохранится автоматически. Ждём до 5 минут...');

const deadline = Date.now() + 5 * 60 * 1000;
let loggedIn = false;
while (Date.now() < deadline) {
  const url = page.url();
  const cookies = await context.cookies('https://hh.ru');
  loggedIn =
    !url.includes('/account/login') ||
    cookies.some((c) => ['hhuid', 'hhrole', 'crypted_id', 'hh_sess_uid'].includes(c.name));
  if (loggedIn) break;
  await page.waitForTimeout(2000);
}

if (loggedIn) {
  const state = await context.storageState({ path: out });
  state.userAgent = await page.evaluate(() => navigator.userAgent).catch(() => UA);
  writeFileSync(out, JSON.stringify(state, null, 2));
  console.log('✓ Сессия сохранена: ' + out);
} else {
  console.error('✗ Не дождался входа (таймаут). Попробуй снова.');
  process.exitCode = 1;
}
await browser.close();
