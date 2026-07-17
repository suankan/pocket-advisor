"""Proper-noun transliteration shadow field (docs_old/specs/transliteration.md).

Scope is deliberately narrow: names/places/organizations only, via a
capitalized-word heuristic, not whole-text transliteration. Ordinary
vocabulary is already well-served by the dense embedding leg; this
exists only to close the specific gap dense embeddings are weak at —
exact-token recall of rare proper nouns across scripts.

Uses `unidecode` (general Unicode->ASCII) rather than a hand-built
Cyrillic table, so the same mechanism covers any future non-Latin
script without extra code — see ROADMAP for the generalization limits
this does NOT solve (romanization-convention mismatches).

Heuristic: capitalized Cyrillic word, any position, excluding a
stopword list of common Russian words that are frequently capitalized
(sentence-initial or for emphasis/greetings). A first version tried
excluding by SENTENCE POSITION instead of a stopword list — wrong:
names are extremely commonly the first word of a sentence ("Ксения
выросла..." / "Xenia grew out of..."), so position-based exclusion
missed exactly the real, verified test case this feature exists for.
Stopwords, empirically gathered from this corpus's actual capitalized-
word frequency distribution (2026-07-12), have no such blind spot.
Inherently imperfect and not exhaustive — acceptable, since this is a
supplementary lexical signal fused with dense+BM25 scoring, not a sole
source of truth.
"""
import re

from unidecode import unidecode

_CAPITALIZED_CYRILLIC = re.compile(r"\b[А-ЯЁ][а-яё]{2,}\b")

_STOPWORDS = {
    "Если", "Общих", "При", "Привет", "Для", "Спасибо", "Это", "Также",
    "Мне", "Общие", "Поэтому", "Предлагаю", "Давай", "Расходов",
    "Пожалуйста", "Расходы", "Развоз", "Все", "Сейчас", "Хорошо",
    "Например", "Дай", "Уборка", "Австралии", "Когда", "Готовка",
    "Россию", "Хочу", "Могу", "Здравствуй", "Добрый", "Уже", "Есть",
    "Надо", "Просто", "Только", "После", "Кстати", "Возможно", "Может",
    "Как", "Что", "Нет", "Да", "Укладывание", "Чтобы", "Однако",
    "Общим", "Гугл", "Далее", "Прошу", "Тогда", "Потому", "Отправление",
    "Каждый", "Именно", "Расходах", "Речь", "Скажи", "Общей", "Стирка",
    "Расходам", "Исключения", "Сразу", "Реестр", "Учитывая",
    "Финансовые", "Австралию", "Октября", "Организация", "Тишина",
    "Срок", "Вместе", "Практически", "Напиши", "Вещи", "Второй",
    "Твое", "Нужно", "Общего", "Она", "Они", "Мы", "Вы", "Ты", "Он",
    "Наши", "Наш", "Наша", "Наше", "Свои", "Свой", "Своя", "Свое",
    "Нашей", "Ваш", "Ваша", "Ваше", "Который", "Которая", "Которые",
}


def proper_noun_shadow(text):
    """Space-separated transliterated tokens for capitalized Cyrillic
    words not in the stopword list. Empty string if nothing qualifies."""
    if not text:
        return ""
    shadows = [unidecode(w) for w in _CAPITALIZED_CYRILLIC.findall(text)
              if w not in _STOPWORDS]
    return " ".join(shadows)
