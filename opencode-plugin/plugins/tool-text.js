import { readFileSync, existsSync } from "node:fs"
import { dirname, join } from "node:path"
import { fileURLToPath } from "node:url"

const dir = join(dirname(fileURLToPath(import.meta.url)), "..", "tool-text")
if (!existsSync(dir)) throw new Error("the factory's tool texts are missing: " + dir + " is not in the baked home")

export default async () => ({
  "tool.definition": async (input, output) => {
    const file = join(dir, input.toolID + ".md")
    if (existsSync(file)) output.description = readFileSync(file, "utf8").trim()
  },
})
