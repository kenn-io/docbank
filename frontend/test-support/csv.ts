export function parseCSV(csv: string): string[][] {
  const records: string[][] = [];
  let record: string[] = [];
  let field = "";
  let quoted = false;
  let closedQuote = false;

  const finishField = (): void => {
    record.push(field);
    field = "";
    closedQuote = false;
  };
  const finishRecord = (): void => {
    finishField();
    records.push(record);
    record = [];
  };

  for (
    let index = csv.startsWith("\uFEFF") ? 1 : 0;
    index < csv.length;
    index += 1
  ) {
    const character = csv[index];
    if (quoted) {
      if (character === '"' && csv[index + 1] === '"') {
        field += '"';
        index += 1;
      } else if (character === '"') {
        quoted = false;
        closedQuote = true;
      } else field += character;
      continue;
    }

    if (closedQuote) {
      if (character === ",") finishField();
      else if (character === "\r" && csv[index + 1] === "\n") {
        finishRecord();
        index += 1;
      } else {
        throw new Error("CSV has an unexpected character after a closing quote");
      }
      continue;
    }

    if (character === '"') {
      if (field !== "") {
        throw new Error("CSV has an unexpected quote in an unquoted field");
      }
      quoted = true;
    } else if (character === ",") finishField();
    else if (character === "\r" && csv[index + 1] === "\n") {
      finishRecord();
      index += 1;
    } else if (character === "\r" || character === "\n") {
      throw new Error("CSV has a bare record separator");
    } else field += character;
  }

  if (quoted) throw new Error("CSV has an unterminated quoted field");
  if (closedQuote || record.length > 0 || field !== "") {
    throw new Error("CSV has an incomplete final record");
  }
  return records;
}
