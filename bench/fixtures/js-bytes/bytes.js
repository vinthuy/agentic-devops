export function formatBytes(n) {
  if (n < 1024) return `${n} B`;
  const kb = Math.round(n / 1024);
  return `${kb} KB`;
}
