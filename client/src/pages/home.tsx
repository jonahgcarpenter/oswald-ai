import Input from "../components/chat/input";

export default function Home() {
  const handleUserMessage = (msg: string) => {
    console.log("User sent:", msg);
  };

  return (
    <div className="flex-grow flex flex-col justify-end">
      {/* In the future, chat messages will go here.
         Give this div 'flex-grow' and 'overflow-y-auto' 
         so messages take up space and scroll while input stays put.
      */}
      <div className="flex-grow"></div>

      <Input onSendMessage={handleUserMessage} />
    </div>
  );
}
