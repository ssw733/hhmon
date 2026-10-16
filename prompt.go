package main

import (
	"fmt"
	"os"
	"strings"
)

func loadProfile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read profile %q: %w", path, err)
	}
	p := strings.TrimSpace(string(data))
	if p == "" {
		return "", fmt.Errorf("profile %q is empty", path)
	}
	return p, nil
}

func buildPrompt(profile string, v Vacancy) (system, user string) {
	system = strings.TrimSpace(`
Ты — опытный карьерный ассистент. Ты пишешь сопроводительные письма для откликов на вакансии hh.ru.

Правила:
- Пиши только на русском языке.
- Тон: живой, уверенный, конкретный. Без клише («команда профессионалов», «динамично развивающаяся компания»).
- Объём: 900–1400 знаков.
- Структура: приветствие, 2–3 предложения почему кандидат подходит именно под эту вакансию (с опорой на реальный опыт из профиля), 1 предложение о мотивации, призыв к диалогу.
- Используй только факты из профиля кандидата. НЕ выдумывай опыт, компании, технологии, цифры.
- Если чего-то в профиле нет — не упоминай это.
- Обращайся нейтрально («Здравствуйте!»), без имени, если имя не указано.
- Не задавай вопросов и не проси дополнительные данные. Всегда выдавай готовое письмо.
- Если в профиле мало данных — пиши на основе того, что есть, не выдумывая факты.
- Не добавляй заголовки, списки и markdown. Только чистый текст письма.
`)

	var b strings.Builder
	b.WriteString("=== ПРОФИЛЬ КАНДИДАТА ===\n")
	b.WriteString(profile)
	b.WriteString("\n\n=== ВАКАНСИЯ ===\n")
	fmt.Fprintf(&b, "Название: %s\n", v.Title)
	if v.Company != "" {
		fmt.Fprintf(&b, "Компания: %s\n", v.Company)
	}
	if v.Region != "" {
		fmt.Fprintf(&b, "Регион: %s\n", v.Region)
	}
	if v.Salary != "" {
		fmt.Fprintf(&b, "Доход: %s\n", v.Salary)
	}
	b.WriteString("Описание:\n")
	desc := v.Description
	if desc == "" {
		desc = "(описание недоступно)"
	}
	if len(desc) > 12000 {
		desc = desc[:12000] + "..."
	}
	b.WriteString(desc)
	b.WriteString("\n\n=== ЗАДАЧА ===\nНапиши сопроводительное письмо для отклика на эту вакансию.")
	return system, b.String()
}
