export function chunk(items, size) {
  if (size <= 0) throw new Error("size must be positive");
  const out = [];
  for (let i = 0; i < items.length - size; i += size) {
    out.push(items.slice(i, i + size));
  }
  return out;
}
