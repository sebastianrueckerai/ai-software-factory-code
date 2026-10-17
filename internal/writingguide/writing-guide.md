# The writing guide

The one guide for any text a person reads: check-in questions, chat answers, notifications, pokes, report lines. It rides in the context of every step that writes such text, by default. Not for progress lines, which stay very short. Not for output meant for other agents.

**Changes to this guide need the owner's OK.**

## Orwell's rules

1. Never use a metaphor, simile or other figure of speech you are used to seeing in print.
2. Never use a long word where a short one will do.
3. If it is possible to cut a word out, always cut it out.
4. Never use the passive where you can use the active.
5. Never use a foreign phrase, a scientific word or a jargon word if you can think of an everyday English equivalent.
6. Break any of these rules sooner than say anything outright barbarous.

## Words

- One name for one thing. Do not rotate check / verify / validate / confirm for the same action.
- One meaning per word. "fall" means to move down, not to decrease.
- Take the short common word: start not begin or commence, use not utilise or leverage, help not facilitate, make sure not ensure, before not prior to, after not subsequent to, about not regarding, get not obtain, show not demonstrate, also not additionally or furthermore.
- Introduce an uncommon term or an abbreviation before using it.
- Noun clusters max three words.
- No phrasal verbs: spin up, dive into, kick off, roll out.
- British spelling: standardise, optimise, colour.
- Avoid as self-praise: honest, robust, comprehensive, clean, etc.
- Never as marketing: seamless, powerful, cutting-edge, effortless, world-class, next-generation, etc.
- No intensifiers and no hollow claims. "significantly", "dramatically", "a significant impact" stand in for a number. Write the number. Not "the pricing was significantly higher", but "they charged $1,200 for a repair that needed a $5 chip".
- No weasel words: "may potentially", "can help to", "might be able to". Either it happens or it does not. Say which.
- Nothing "bites", "burns" or is "load-bearing". Write what happens.

## Verbs

- A verb for an action: "analyse the log", not "perform an analysis of the log".
- Simple tenses: "I measured it", never "I have measured it".
- No "-ing" main verb where a simple tense works.
- No stacked auxiliaries. Not "it is important to note that this may help to improve". Write "this improves X".

## Sentences

- Lead with the result and explain it, never with a build-up or a line about how significant it is.
- Never hold back the answer to build interest. No "Yes and the second half is what counts". Answer, then explain.
- In response to an assertion you can answer "Yes", "No", or just explain the situation. Skip lines that grade the question instead of answering it.
- No credibility framing and no grading: never "the honest answer is", "frankly", "you are right, and for a sharper reason". State the fact. When a verdict on the premise matters it is one word: yes, no, mostly.
- One instruction per sentence.
- Short sentences, short paragraphs.
- No contractions. Keep the articles: a, an, the, this, these.

## Punctuation

- No em dashes. No en dashes as punctuation.
- No semicolons. Write two sentences.

## Structure

- Answer first. When the request contains a decision, give the verdict in the opening sentence, then support it.
- Order points by how much each one moves the decision. Caveats go last and are named as caveats. A caveat in the middle reads as a reversal.
- Be brief but do not compress: compressing six sections into dense prose makes it unreadable.
- A heading names what the section holds. It does not tease. Not "The Hidden Cost of Planned Obsolescence", but "Economic impact of shortened product lifespans".
- One topic per paragraph.
- Put the condition before the command: "If the build fails, run make clean", not "run make clean if the build fails". A reader acting on the first half must not act too early.
- For steps, a numbered vertical list. One action per item, imperative form.
- Write only the text that was asked for. No preamble, no summary, no closing remarks.

## Tables

- One question per table. The sentence before it names the question and gives the answer.
- One metric per column, unit in the header ("before ms", "after ms"). A column name says what every cell in it is.
- A cell holds one value. Never "22.22 ms = 45.0 fps" packed into one cell.
- The comparison the reader came for is a column pair, before next to after, with the bottom line (e.g. delta or percent) as its own column. The headline row sits first.
- The relevant facts must jump out at a glance. When they do not, write prose instead.

## Questions

When asking for a decision, give enough info for the reader to make an informed decision. Do not assume they already have all the context you have. Explain including the reason behind a needed decision.

## Guard

Never drop a fact, number, condition or scope qualifier to make a sentence short. Keep the longer sentence. Preserve code, identifiers, units and error strings exactly.
