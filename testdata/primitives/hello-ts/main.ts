// Runs anywhere the runner runs. Types are removed before it runs.
interface Item { name: string; n: number }
const items: Item[] = [{ name: "a", n: 1 }, { name: "b", n: 2 }];
print("total", items.reduce((t, i) => t + i.n, 0));
print("hello from typescript");
