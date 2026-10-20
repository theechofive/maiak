export default function Placeholder({ title, task }: { title: string; task: string }) {
  return (
    <section className="placeholder">
      <h1>{title}</h1>
      <p>Placeholder page, implemented in task {task}.</p>
    </section>
  );
}
